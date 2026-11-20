package queue

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/WyattJia/youtube-playlist-cli/internal/fsutil"
)

type RetryResult struct {
	Tracks    int `json:"tracks"`
	Playlists int `json:"playlists"`
}

func (s Store) RetryFailed(includePermanent bool) (RetryResult, error) {
	var result RetryResult
	err := s.withLock(func() error {
		q, err := s.load()
		if err != nil {
			return err
		}
		for i := range q.Playlists {
			p := &q.Playlists[i]
			if p.PendingInsert != nil || p.Status == Conflict || p.Status == Full && !includePermanent {
				continue
			}
			retry := includePermanent && p.Status == Full
			for j := range p.Tracks {
				t := &p.Tracks[j]
				if t.Status == TrackTransient || includePermanent && t.Status == TrackPermanent {
					t.Status, t.ErrorCode, t.Error = TrackPending, "", ""
					result.Tracks++
					retry = true
				}
			}
			if retry {
				p.Status, p.Error = Pending, ""
				result.Playlists++
			}
		}
		if result.Playlists == 0 {
			return nil
		}
		return s.save(&q)
	})
	return result, err
}

func (s Store) ExportReport(path, format string) error {
	if path == "" {
		return errors.New("需要 --out <文件路径>")
	}
	if format != "csv" && format != "json" {
		return errors.New("--format 必须是 csv 或 json")
	}
	return s.withLock(func() error {
		for _, statePath := range []string{s.path, s.lock} {
			same, err := sameFile(path, statePath)
			if err != nil {
				return err
			}
			if same {
				return errors.New("报告不能覆盖队列文件或队列锁")
			}
		}
		q, err := s.load()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("创建报告目录: %w", err)
		}
		if format == "json" {
			return fsutil.WriteJSON(path, q, 0o600)
		}

		var buf bytes.Buffer
		rows := csv.NewWriter(&buf)
		rows.UseCRLF = true
		if err := rows.Write([]string{
			"source_commit", "netease_playlist_id", "name", "youtube_playlist_id", "playlist_status",
			"creation_pending", "extra_remote_tracks_count", "playlist_error", "netease_song_id",
			"video_id", "match", "track_status", "track_error_code", "track_error",
		}); err != nil {
			return err
		}
		for _, p := range q.Playlists {
			tracks := p.Tracks
			if len(tracks) == 0 {
				tracks = []Track{{}}
			}
			for _, t := range tracks {
				if err := fsutil.WriteCSVRow(rows,
					q.SourceCommit, p.SourceID, p.Name, p.YouTubeID, string(p.Status),
					strconv.FormatBool(p.CreationPending), strconv.Itoa(p.ExtraRemoteTracksCount), p.Error,
					t.SourceID, t.VideoID, t.Match, string(t.Status), t.ErrorCode, t.Error,
				); err != nil {
					return err
				}
			}
		}
		rows.Flush()
		if err := rows.Error(); err != nil {
			return err
		}
		return fsutil.AtomicWrite(path, buf.Bytes(), 0o600)
	})
}
