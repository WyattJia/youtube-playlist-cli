package queue

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/WyattJia/youtube-playlist-cli/internal/quota"
	"github.com/WyattJia/youtube-playlist-cli/internal/videoid"
	"github.com/WyattJia/youtube-playlist-cli/internal/youtube"
)

type API interface {
	ListMyPlaylists(context.Context) ([]youtube.Playlist, error)
	CreatePlaylist(ctx context.Context, title, description, privacy string) (youtube.Playlist, error)
	ListPlaylistItems(ctx context.Context, id string) ([]youtube.PlaylistItem, error)
	InsertPlaylistItem(ctx context.Context, playlistID, videoID string, position *int) (youtube.PlaylistItem, error)
}

type RunOptions struct {
	Playlist        string
	DryRun          bool
	ConfirmRecreate bool
	ConfirmReinsert bool
	Now             func() time.Time
}

type Stop struct {
	Code       string     `json:"code"`
	Message    string     `json:"message"`
	ResetAt    *time.Time `json:"resetAt,omitempty"`
	RetryAfter *time.Time `json:"retryAfter,omitempty"`
}

type RunResult struct {
	DryRun         bool          `json:"dryRun"`
	Created        int           `json:"created"`
	Recovered      int           `json:"recovered"`
	Inserted       int           `json:"inserted"`
	PlannedCreates int           `json:"plannedCreates"`
	PlannedInserts int           `json:"plannedInserts"`
	Playlists      []RunPlaylist `json:"playlists"`
	Stopped        *Stop         `json:"stopped,omitempty"`
}

type RunPlaylist struct {
	SourceID               string              `json:"neteasePlaylistId"`
	YouTubeID              string              `json:"youtubePlaylistId,omitempty"`
	Status                 PlaylistStatus      `json:"status"`
	Error                  string              `json:"error,omitempty"`
	CreationPending        bool                `json:"creationPending"`
	PendingInsert          *PendingInsert      `json:"pendingInsert,omitempty"`
	ExtraRemoteTracksCount int                 `json:"extraRemoteTracksCount"`
	TrackCounts            map[TrackStatus]int `json:"trackCounts"`
}

type runner struct {
	store     Store
	api       API
	q         *State
	opts      RunOptions
	report    *RunResult
	transient int
}

func (s Store) Run(ctx context.Context, api API, opts RunOptions) (RunResult, error) {
	report := RunResult{DryRun: opts.DryRun, Playlists: []RunPlaylist{}}
	if opts.ConfirmRecreate && opts.Playlist == "" {
		return report, errors.New("--confirm-recreate 必须与 --playlist <网易云歌单 ID> 一起使用")
	}
	if opts.ConfirmReinsert && opts.Playlist == "" {
		return report, errors.New("--confirm-reinsert 必须与 --playlist <网易云歌单 ID> 一起使用")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	err := s.withLock(func() error {
		q, err := s.load()
		if err != nil {
			return err
		}
		if opts.Playlist != "" && !slices.ContainsFunc(q.Playlists, func(p Playlist) bool {
			return p.SourceID == opts.Playlist
		}) {
			return errors.New("--playlist 指定的网易云歌单不在队列中")
		}

		r := runner{store: s, api: api, q: &q, opts: opts, report: &report}
		if q.RetryAfter != nil {
			if opts.Now().Before(*q.RetryAfter) {
				report.Stopped = retryAfterStop(*q.RetryAfter)
				return nil
			}
			q.RetryAfter = nil
			if err := r.persist(); err != nil {
				return err
			}
		}
		for i := range q.Playlists {
			p := &q.Playlists[i]
			if opts.Playlist != "" && p.SourceID != opts.Playlist {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			err := r.process(ctx, p)
			report.Playlists = append(report.Playlists, runPlaylist(p))
			if err != nil {
				stop := cleanStop(err)
				if stop == nil {
					return err
				}
				report.Stopped = stop
			}
			if report.Stopped != nil {
				break
			}
		}
		return nil
	})
	return report, err
}

func runPlaylist(p *Playlist) RunPlaylist {
	counts := make(map[TrackStatus]int)
	for _, t := range p.Tracks {
		counts[t.Status]++
	}
	return RunPlaylist{
		SourceID: p.SourceID, YouTubeID: p.YouTubeID, Status: p.Status, Error: p.Error,
		CreationPending: p.CreationPending, PendingInsert: p.PendingInsert,
		ExtraRemoteTracksCount: p.ExtraRemoteTracksCount, TrackCounts: counts,
	}
}

func retryAfterStop(until time.Time) *Stop {
	return &Stop{
		Code: string(youtube.Transient), RetryAfter: &until,
		Message: "服务端要求暂停请求。请在 " + until.Format(time.RFC3339) + " 后重试",
	}
}

func cleanStop(err error) *Stop {
	var budget *quota.BudgetError
	if errors.As(err, &budget) {
		code := "BUDGET_EXHAUSTED"
		if budget.Writes {
			code = "MAX_WRITES"
		}
		return &Stop{Code: code, Message: err.Error()}
	}
	switch kind := youtube.KindOf(err); kind {
	case youtube.QuotaExceeded, youtube.AuthExpired, youtube.AuthRequired, youtube.PermissionDenied:
		stop := &Stop{Code: string(kind), Message: err.Error()}
		var limited *quota.LimitError
		var apiErr *youtube.APIError
		switch {
		case errors.As(err, &limited):
			stop.ResetAt = &limited.ResetAt
		case errors.As(err, &apiErr) && !apiErr.ResetAt.IsZero():
			stop.ResetAt = &apiErr.ResetAt
		}
		return stop
	default:
		return nil
	}
}

func (r *runner) process(ctx context.Context, p *Playlist) error {
	if p.Status == Conflict || p.Status == Full {
		return nil
	}
	ready, err := r.prepareTarget(ctx, p)
	if err != nil || !ready {
		return err
	}
	var items []youtube.PlaylistItem
	if p.YouTubeID != "" {
		items, err = r.api.ListPlaylistItems(ctx, p.YouTubeID)
		if err != nil {
			return r.playlistFailure(p, err)
		}
	}

	tracks := plannedTrackIDs(p)
	counts := remoteVideoCounts(items)
	confirmedInsert := -1
	if pending := p.PendingInsert; pending != nil {
		switch {
		case tracks[pending.TrackIndex].remoteCount(counts) >= pending.ExpectedMinCount:
			confirmedInsert = pending.TrackIndex
		case !r.opts.ConfirmReinsert:
			p.Status = Paused
			p.Error = fmt.Sprintf("视频 %s 的插入结果尚未在远端可见，请稍后重试。确认未插入后，可使用 --playlist %s --confirm-reinsert",
				pending.VideoID, p.SourceID)
			return r.persist()
		}
		p.PendingInsert = nil
	}

	matched, count := matchingTracks(p, tracks, counts, confirmedInsert)
	p.ExtraRemoteTracksCount = len(items) - count
	for i := range p.Tracks {
		t := &p.Tracks[i]
		if matched[i] {
			t.Status, t.ErrorCode, t.Error = TrackDone, "", ""
		} else if t.Status == TrackDone {
			t.Status = TrackPending
		}
	}
	p.Error = ""
	updateStatus(p)
	if err := r.persist(); err != nil {
		return err
	}

	for i := range p.Tracks {
		t := &p.Tracks[i]
		if t.Status != TrackPending && t.Status != TrackTransient {
			continue
		}
		if r.opts.DryRun {
			r.report.PlannedInserts++
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		p.PendingInsert = &PendingInsert{TrackIndex: i, VideoID: t.VideoID, ExpectedMinCount: tracks[i].remoteCount(counts) + 1}
		p.Status, p.Error = Paused, "条目插入结果待确认"
		if err := r.persist(); err != nil {
			return err
		}

		item, err := r.api.InsertPlaylistItem(ctx, p.YouTubeID, t.VideoID, nil)
		var apiErr *youtube.APIError
		errors.As(err, &apiErr)
		ambiguous := apiErr != nil && apiErr.Ambiguous
		if !ambiguous {
			// The request succeeded, was rejected, or never reached the API.
			p.PendingInsert = nil
		}
		pause := ambiguous || apiErr != nil && apiErr.RetryAfter > 0
		kind := youtube.KindOf(err)
		switch {
		case err == nil:
			actualID := cmp.Or(item.VideoID(), t.VideoID)
			t.Status, t.ErrorCode, t.Error = TrackDone, "", ""
			if actualID != t.VideoID && videoid.ValidVideo(actualID) {
				t.ActualVideoID = actualID
				tracks[i].equivalent = actualID
				tracks[i].verified = true
			} else {
				t.ActualVideoID = ""
			}
			counts[actualID]++
			r.report.Inserted++
			r.transient = 0
		case pause, kind == youtube.Transient:
			t.Status, t.ErrorCode, t.Error = TrackTransient, string(kind), err.Error()
			if pause {
				return r.playlistFailure(p, err)
			}
			r.noteTransient(err)
		case apiErr != nil && apiErr.IsPermanentTrackError():
			t.Status, t.ErrorCode, t.Error = TrackPermanent, string(kind), err.Error()
			r.transient = 0
		default:
			return r.playlistFailure(p, err)
		}
		p.Error = ""
		updateStatus(p)
		if err := r.persist(); err != nil {
			return err
		}
		if r.report.Stopped != nil {
			break
		}
	}
	return nil
}

func (r *runner) prepareTarget(ctx context.Context, p *Playlist) (bool, error) {
	marker := sourceMarker(p.SourceID, r.q.SourceCommit)
	wasPending := p.CreationPending
	if wasPending {
		remote, err := r.api.ListMyPlaylists(ctx)
		if err != nil {
			return false, r.playlistFailure(p, err)
		}
		var matches []string
		for _, candidate := range remote {
			if candidate.Snippet.Title == p.Name && strings.Contains(candidate.Snippet.Description, marker) {
				if !videoid.ValidPlaylist(candidate.ID) {
					return false, errors.New("找到来源标记，但远端歌单 ID 无效")
				}
				if !slices.Contains(matches, candidate.ID) {
					matches = append(matches, candidate.ID)
				}
			}
		}
		switch {
		case len(matches) > 1, len(matches) == 1 && p.YouTubeID != "" && p.YouTubeID != matches[0]:
			if p.YouTubeID != "" && !slices.Contains(matches, p.YouTubeID) {
				matches = append(matches, p.YouTubeID)
			}
			slices.Sort(matches)
			p.ConflictingYouTubeIDs = matches
			p.Status, p.Error = Conflict, "来源标记命中多个可能的目标，请核对歌单 ID 后重新初始化"
			return false, r.persist()
		case len(matches) == 1:
			p.YouTubeID, p.CreationPending = matches[0], false
			p.Status, p.Error = Pending, ""
			r.report.Recovered++
			return true, r.persist()
		case p.YouTubeID != "":
			p.Status, p.Error = Paused, "已有计划目标 ID，但旧创建记录尚未通过远端来源标记确认"
			return false, r.persist()
		case !r.opts.ConfirmRecreate:
			p.Status = Paused
			p.Error = "远端还未显示来源标记，保留待确认状态。请稍后重试，确认远端不存在后才使用 --confirm-recreate"
			return false, r.persist()
		}
	}
	if p.YouTubeID != "" {
		return true, nil
	}
	if r.opts.DryRun {
		r.report.PlannedCreates++
		return true, nil
	}

	p.CreationPending = true
	p.Status, p.Error = Paused, "创建结果待确认"
	if err := r.persist(); err != nil {
		return false, err
	}
	created, err := r.api.CreatePlaylist(ctx, p.Name, marker, "private")
	if err != nil {
		var apiErr *youtube.APIError
		p.CreationPending = wasPending || errors.As(err, &apiErr) && apiErr.Ambiguous
		return false, r.playlistFailure(p, err)
	}
	if !videoid.ValidPlaylist(created.ID) {
		err := &youtube.APIError{Kind: youtube.Transient, Ambiguous: true, Message: "创建请求未返回有效的目标 ID"}
		return false, r.playlistFailure(p, err)
	}
	p.YouTubeID, p.CreationPending = created.ID, false
	p.Status, p.Error = Pending, ""
	r.report.Created++
	r.transient = 0
	return true, r.persist()
}

func updateStatus(p *Playlist) {
	p.Status = Done
	for _, t := range p.Tracks {
		if t.Status != TrackDone && t.Status != TrackUnmatched {
			p.Status = Partial
			break
		}
	}
}

func (r *runner) playlistFailure(p *Playlist, cause error) error {
	p.Error = cause.Error()
	var apiErr *youtube.APIError
	if errors.As(cause, &apiErr) && apiErr.RetryAfter > 0 {
		until := apiErr.RetryAt
		if until.IsZero() {
			until = r.opts.Now().UTC().Add(apiErr.RetryAfter)
		}
		p.Status = Paused
		r.q.RetryAfter = &until
		r.report.Stopped = retryAfterStop(until)
		return r.persistRetryAfter(until)
	}
	switch youtube.KindOf(cause) {
	case youtube.PlaylistFull:
		p.Status = Full
		r.transient = 0
		return r.persist()
	case youtube.PlaylistNotFound, youtube.ItemNotFound:
		p.Status = Failed
		r.transient = 0
		return r.persist()
	case youtube.Transient:
		p.Status = Paused
		r.noteTransient(cause)
		return r.persist()
	}
	p.Status = Paused
	if err := r.persist(); err != nil {
		return fmt.Errorf("记录远端操作暂停失败 (%s): %w", cause, err)
	}
	return cause
}

func (r *runner) noteTransient(err error) {
	r.transient++
	if r.transient >= 3 {
		r.report.Stopped = &Stop{Code: string(youtube.Transient), Message: "连续 3 次遇到暂时性失败，已保存进度。最近一次失败为 " + err.Error()}
	}
}

func (r *runner) persist() error {
	if r.opts.DryRun {
		return nil
	}
	return r.store.save(r.q)
}

func (r *runner) persistRetryAfter(until time.Time) error {
	if !r.opts.DryRun {
		return r.store.save(r.q)
	}
	persisted, err := r.store.load()
	if err != nil {
		return err
	}
	persisted.RetryAfter = &until
	return r.store.save(&persisted)
}
