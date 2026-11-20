// Package queue imports migration plans and resumes them against remote state.
package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WyattJia/youtube-playlist-cli/internal/fsutil"
	"github.com/WyattJia/youtube-playlist-cli/internal/videoid"
)

const version = 1

type PlaylistStatus string

const (
	Pending  PlaylistStatus = "pending"
	Paused   PlaylistStatus = "paused"
	Partial  PlaylistStatus = "partial"
	Done     PlaylistStatus = "done"
	Conflict PlaylistStatus = "conflict"
	Full     PlaylistStatus = "full"
	Failed   PlaylistStatus = "failed"
)

type TrackStatus string

const (
	TrackPending   TrackStatus = "pending"
	TrackUnmatched TrackStatus = "unmatched"
	TrackDone      TrackStatus = "done"
	TrackPermanent TrackStatus = "failed_permanent"
	TrackTransient TrackStatus = "failed_transient"
)

type Track struct {
	SourceID      string      `json:"neteaseSongId"`
	VideoID       string      `json:"videoId,omitempty"`
	ActualVideoID string      `json:"actualVideoId,omitempty"`
	Match         string      `json:"match,omitempty"`
	Status        TrackStatus `json:"status"`
	ErrorCode     string      `json:"errorCode,omitempty"`
	Error         string      `json:"error,omitempty"`
}

type PendingInsert struct {
	TrackIndex       int    `json:"trackIndex"`
	VideoID          string `json:"videoId"`
	ExpectedMinCount int    `json:"expectedMinCount"`
}

type Playlist struct {
	SourceID                  string         `json:"neteasePlaylistId"`
	Name                      string         `json:"name"`
	YouTubeID                 string         `json:"youtubePlaylistId,omitempty"`
	PlanYouTubeID             string         `json:"planYouTubePlaylistId,omitempty"`
	RecordYouTubeID           string         `json:"recordYouTubePlaylistId,omitempty"`
	ConflictingYouTubeIDs     []string       `json:"conflictingYouTubePlaylistIds,omitempty"`
	CreationPending           bool           `json:"creationPending"`
	Status                    PlaylistStatus `json:"status"`
	Error                     string         `json:"error,omitempty"`
	ExtraRemoteTracksCount    int            `json:"extraRemoteTracksCount"`
	VerifiedRequestedVideoIDs []string       `json:"verifiedRequestedVideoIds,omitempty"`
	VerifiedActualVideoIDs    []string       `json:"verifiedActualVideoIds,omitempty"`
	PendingInsert             *PendingInsert `json:"pendingInsert,omitempty"`
	Tracks                    []Track        `json:"tracks"`
}

type State struct {
	Version      int        `json:"version"`
	SourceCommit string     `json:"sourceCommit"`
	Playlists    []Playlist `json:"playlists"`
	RetryAfter   *time.Time `json:"retryAfter,omitempty"`
}

type Store struct {
	path string
	lock string
}

func New(stateDir, file string) Store {
	if file == "" {
		file = filepath.Join(stateDir, "migration-queue.json")
	}
	return Store{path: file, lock: filepath.Join(stateDir, "queue.lock")}
}

func (s Store) Status() (State, error) {
	var state State
	err := s.withLock(func() error {
		var err error
		state, err = s.load()
		return err
	})
	return state, err
}

func (s Store) withLock(fn func() error) error {
	same, err := sameFile(s.path, s.lock)
	if err != nil {
		return err
	}
	if same {
		return errors.New("队列文件不能与队列锁使用同一文件")
	}
	if err := os.MkdirAll(filepath.Dir(s.lock), 0o700); err != nil {
		return fmt.Errorf("创建队列状态目录: %w", err)
	}
	return fsutil.WithFileLock(s.lock, fn)
}

func (s Store) load() (State, error) {
	var q State
	if err := readJSON(s.path, "迁移队列", &q); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return q, errors.New("没有迁移队列，请先运行 ytpl migrate init")
		}
		return q, err
	}
	if err := q.validate(); err != nil {
		return q, err
	}
	return q, nil
}

func (s Store) save(q *State) error {
	if err := fsutil.WriteJSON(s.path, q, 0o600); err != nil {
		return fmt.Errorf("保存迁移队列: %w", err)
	}
	return nil
}

func (q *State) validate() error {
	if q.Version != version || q.Playlists == nil {
		return errors.New("不支持的迁移队列格式")
	}
	if q.SourceCommit != "" && !videoid.ValidSource(q.SourceCommit) {
		return errors.New("迁移队列含无效的备份提交标识")
	}
	seen := make(map[string]bool, len(q.Playlists))
	for _, p := range q.Playlists {
		if !videoid.ValidSource(p.SourceID) || seen[p.SourceID] || strings.TrimSpace(p.Name) == "" {
			return errors.New("迁移队列含无效或重复的网易云歌单")
		}
		seen[p.SourceID] = true
		switch p.Status {
		case Pending, Paused, Partial, Done, Conflict, Full, Failed:
		default:
			return fmt.Errorf("歌单 %s 的状态无效", p.SourceID)
		}
		for _, id := range []string{p.YouTubeID, p.PlanYouTubeID, p.RecordYouTubeID} {
			if id != "" && !videoid.ValidPlaylist(id) {
				return fmt.Errorf("歌单 %s 含无效的 YouTube 歌单 ID", p.SourceID)
			}
		}
		if p.PlanYouTubeID != "" && p.RecordYouTubeID != "" && p.PlanYouTubeID != p.RecordYouTubeID && p.Status != Conflict {
			return fmt.Errorf("歌单 %s 的计划和记录目标冲突", p.SourceID)
		}
		for _, track := range p.Tracks {
			if track.VideoID == "" && track.Status == TrackUnmatched {
				continue
			}
			if !videoid.ValidVideo(track.VideoID) {
				return fmt.Errorf("歌单 %s 含无效的 YouTube 视频 ID", p.SourceID)
			}
			if track.ActualVideoID != "" && !videoid.ValidVideo(track.ActualVideoID) {
				return fmt.Errorf("歌单 %s 含无效的远端等价视频 ID", p.SourceID)
			}
			switch track.Status {
			case TrackPending, TrackDone, TrackPermanent, TrackTransient:
			default:
				return fmt.Errorf("歌单 %s 含无效的曲目状态", p.SourceID)
			}
		}
		if len(p.VerifiedRequestedVideoIDs) != len(p.VerifiedActualVideoIDs) {
			return fmt.Errorf("歌单 %s 的验证映射数量不一致", p.SourceID)
		}
		for i, id := range p.VerifiedRequestedVideoIDs {
			if !videoid.ValidVideo(id) || !videoid.ValidVideo(p.VerifiedActualVideoIDs[i]) {
				return fmt.Errorf("歌单 %s 的验证映射含无效的视频 ID", p.SourceID)
			}
		}
		if pending := p.PendingInsert; pending != nil {
			if p.YouTubeID == "" || p.CreationPending || pending.ExpectedMinCount < 1 ||
				pending.TrackIndex < 0 || pending.TrackIndex >= len(p.Tracks) ||
				pending.VideoID == "" || p.Tracks[pending.TrackIndex].VideoID != pending.VideoID {
				return fmt.Errorf("歌单 %s 的待确认插入记录无效", p.SourceID)
			}
		}
	}
	return nil
}

func (p Playlist) requested() []string {
	var ids []string
	for _, t := range p.Tracks {
		if t.VideoID != "" {
			ids = append(ids, t.VideoID)
		}
	}
	return ids
}

func readJSON(path, label string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取%s: %w", label, err)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("解析%s JSON: %w", label, err)
	}
	return nil
}

func sameFile(a, b string) (bool, error) {
	first, err := filepath.Abs(a)
	if err != nil {
		return false, fmt.Errorf("解析状态路径: %w", err)
	}
	second, err := filepath.Abs(b)
	if err != nil {
		return false, fmt.Errorf("解析状态路径: %w", err)
	}
	if first == second {
		return true, nil
	}
	x, err := os.Stat(first)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("检查状态文件: %w", err)
	}
	y, err := os.Stat(second)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("检查状态文件: %w", err)
	}
	return os.SameFile(x, y), nil
}
