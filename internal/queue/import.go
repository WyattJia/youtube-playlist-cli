package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/WyattJia/youtube-playlist-cli/internal/videoid"
)

type InitOptions struct {
	Plan    string
	Record  string
	Include []string
	Exclude []string
	Force   bool
}

type planFile struct {
	SourceCommit string `json:"sourceCommit"`
	Playlists    []struct {
		SourceID  string `json:"neteasePlaylistId"`
		Name      string `json:"name"`
		YouTubeID string `json:"youtubePlaylistId"`
		Tracks    []struct {
			SourceID string `json:"neteaseSongId"`
			VideoID  string `json:"videoId"`
			Match    string `json:"match"`
		} `json:"tracks"`
	} `json:"playlists"`
}

type legacyPlaylist struct {
	Creating          bool     `json:"creating"`
	YouTubeID         string   `json:"youtubeId"`
	Verified          bool     `json:"verified"`
	VideoIDs          []string `json:"videoIds"`
	RequestedVideoIDs []string `json:"requestedVideoIds"`
}

type recordFile struct {
	Commit    string
	Playlists map[string]legacyPlaylist
}

func (s Store) Init(opts InitOptions) (State, error) {
	var q State
	if opts.Plan == "" {
		return q, errors.New("需要 --plan <迁移计划 JSON>")
	}
	err := s.withLock(func() error {
		for _, source := range []string{opts.Plan, opts.Record} {
			if source == "" {
				continue
			}
			for _, destination := range []string{s.path, s.lock} {
				same, err := sameFile(source, destination)
				if err != nil {
					return err
				}
				if same {
					return errors.New("队列或锁路径不能指向迁移计划或旧迁移记录")
				}
			}
		}
		if !opts.Force {
			_, err := os.Lstat(s.path)
			if err == nil {
				return errors.New("迁移队列已存在，使用 --force 覆盖")
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("检查迁移队列文件: %w", err)
			}
		}

		var err error
		q, err = importState(opts)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
			return fmt.Errorf("创建迁移队列目录: %w", err)
		}
		return s.save(&q)
	})
	return q, err
}

func importState(opts InitOptions) (State, error) {
	var plan planFile
	if err := readJSON(opts.Plan, "迁移计划", &plan); err != nil {
		return State{}, err
	}
	if plan.Playlists == nil {
		return State{}, errors.New("迁移计划缺少 playlists 列表")
	}
	record, err := readRecord(opts.Record)
	if err != nil {
		return State{}, err
	}
	if plan.SourceCommit != "" && record.Commit != "" && plan.SourceCommit != record.Commit {
		return State{}, errors.New("迁移计划 sourceCommit 与旧记录 commit 不一致，请使用同一次备份的文件")
	}
	commit := plan.SourceCommit
	if commit == "" {
		commit = record.Commit
	}
	q := State{Version: version, SourceCommit: commit, Playlists: []Playlist{}}

	available := make(map[string]bool)
	for _, p := range plan.Playlists {
		available[p.SourceID] = true
	}
	for _, selection := range [][]string{opts.Include, opts.Exclude} {
		for _, id := range selection {
			if !videoid.ValidSource(id) {
				return State{}, errors.New("筛选项含无效的网易云歌单 ID")
			}
			if !available[id] {
				return State{}, fmt.Errorf("迁移计划中没有要筛选的歌单 %s", id)
			}
		}
	}

	for _, p := range plan.Playlists {
		if (len(opts.Include) > 0 && !slices.Contains(opts.Include, p.SourceID)) || slices.Contains(opts.Exclude, p.SourceID) {
			continue
		}
		tracks := make([]Track, 0, len(p.Tracks))
		for _, original := range p.Tracks {
			t := Track{SourceID: original.SourceID, VideoID: original.VideoID, Match: original.Match, Status: TrackPending}
			if t.VideoID == "" {
				t.Status = TrackUnmatched
			}
			tracks = append(tracks, t)
		}

		legacy := record.Playlists[p.SourceID]
		target := Playlist{
			SourceID: p.SourceID, Name: p.Name, YouTubeID: p.YouTubeID, PlanYouTubeID: p.YouTubeID,
			RecordYouTubeID: legacy.YouTubeID, Status: Pending, Tracks: tracks,
		}
		if p.YouTubeID != "" && legacy.YouTubeID != "" && p.YouTubeID != legacy.YouTubeID {
			target.Status = Conflict
			target.Error = fmt.Sprintf("计划目标 %s 与旧记录目标 %s 不一致", p.YouTubeID, legacy.YouTubeID)
			target.YouTubeID = ""
			target.ConflictingYouTubeIDs = []string{p.YouTubeID, legacy.YouTubeID}
		} else {
			if target.YouTubeID == "" {
				target.YouTubeID = legacy.YouTubeID
			}
			target.CreationPending = legacy.Creating && legacy.YouTubeID == ""
			if target.CreationPending {
				target.Status = Paused
				target.Error = "旧记录显示可能已经创建歌单，需查找远端来源标记"
			}
		}
		importVerification(&target, legacy)
		q.Playlists = append(q.Playlists, target)
	}
	if err := q.validate(); err != nil {
		return State{}, err
	}
	return q, nil
}

func readRecord(path string) (recordFile, error) {
	var record recordFile
	if path == "" {
		return record, nil
	}
	var fields map[string]json.RawMessage
	if err := readJSON(path, "旧迁移记录", &fields); err != nil {
		return record, err
	}
	if err := json.Unmarshal(fields["playlists"], &record.Playlists); err != nil || record.Playlists == nil {
		return record, errors.New("旧迁移记录 (--record) 必须包含非 null 的 playlists JSON 对象")
	}
	if commit, ok := fields["commit"]; ok {
		if err := json.Unmarshal(commit, &record.Commit); err != nil {
			return record, fmt.Errorf("解析旧迁移记录 commit: %w", err)
		}
	}
	return record, nil
}

func importVerification(p *Playlist, legacy legacyPlaylist) {
	if !legacy.Verified {
		return
	}
	planned := p.requested()
	requested := legacy.RequestedVideoIDs
	if len(requested) == 0 && slices.Equal(planned, legacy.VideoIDs) {
		requested = legacy.VideoIDs
	}
	if len(requested) != len(legacy.VideoIDs) {
		return
	}
	p.VerifiedRequestedVideoIDs = slices.Clone(requested)
	p.VerifiedActualVideoIDs = slices.Clone(legacy.VideoIDs)
	if p.Status != Pending || p.YouTubeID == "" || !slices.Equal(planned, requested) {
		return
	}
	p.Status = Done
	for i := range p.Tracks {
		if p.Tracks[i].Status == TrackPending {
			p.Tracks[i].Status = TrackDone
		}
	}
}

func sourceMarker(id, commit string) string {
	if commit == "" {
		commit = "未知"
	} else if len(commit) > 7 {
		commit = commit[:7]
	}
	return "网易云音乐歌单 ID " + id + " · 备份 " + commit
}
