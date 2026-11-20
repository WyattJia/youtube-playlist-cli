package playlist

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/WyattJia/youtube-playlist-cli/internal/fsutil"
	"github.com/WyattJia/youtube-playlist-cli/internal/store"
	"github.com/WyattJia/youtube-playlist-cli/internal/videoid"
)

type Exported struct {
	PlaylistID string `json:"playlistId"`
	Path       string `json:"path"`
	Skipped    int    `json:"skipped,omitempty"`
}

func (s Service) Export(ctx context.Context, id, out, format string, remote bool) ([]Exported, error) {
	result := []Exported{}
	if out == "" {
		return result, errors.New("需要 --out <目录>")
	}
	if format != "json" && format != "csv" && format != "txt" {
		return result, errors.New("--format 必须是 json、csv 或 txt")
	}

	ids := []string{id}
	if id == "" {
		playlists, err := s.List(ctx, remote)
		if err != nil {
			return result, err
		}
		ids = make([]string, 0, len(playlists))
		for _, p := range playlists {
			ids = append(ids, p.ID)
		}
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		return result, fmt.Errorf("创建导出目录: %w", err)
	}
	for _, id := range ids {
		entry, err := s.Show(ctx, id, remote)
		if err != nil {
			return result, err
		}
		if !entry.HasItems {
			return result, fmt.Errorf("歌单 %s 没有本地条目快照，请先同步条目或使用 --remote", id)
		}
		path := filepath.Join(out, exportFilename(entry.Playlist.Snippet.Title, id, format))
		skipped, err := writeExport(path, format, entry)
		if err != nil {
			return result, fmt.Errorf("导出歌单 %s: %w", id, err)
		}
		result = append(result, Exported{PlaylistID: id, Path: path, Skipped: skipped})
	}
	return result, nil
}

func exportFilename(title, id, format string) string {
	title = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune("/\\:*?\"<>|", r) {
			return '_'
		}
		return r
	}, title)
	title = strings.Trim(strings.TrimSpace(title), ". ")
	// Distinguish IDs that differ only in case on case-insensitive volumes.
	digest := sha256.Sum256([]byte(id))
	suffix := fmt.Sprintf("--%s-%x.%s", id, digest[:6], format)
	limit := 200 - len(suffix)
	if len(title) > limit {
		for !utf8.RuneStart(title[limit]) {
			limit--
		}
		title = title[:limit]
	}
	title = strings.TrimRight(title, ". ")
	if title == "" {
		title = "playlist"
	}
	return title + suffix
}

func writeExport(path, format string, entry store.Entry) (skipped int, err error) {
	if format == "json" {
		return 0, fsutil.WriteJSON(path, entry, 0o600)
	}
	var data bytes.Buffer
	if format == "csv" {
		rows := csv.NewWriter(&data)
		rows.UseCRLF = true
		if err := rows.Write([]string{"position", "playlist_item_id", "video_id", "title", "channel", "added_at", "url"}); err != nil {
			return 0, err
		}
		for _, item := range entry.Items {
			if err := fsutil.WriteCSVRow(rows,
				strconv.Itoa(item.Snippet.Position), item.ID, item.VideoID(), item.Snippet.Title,
				item.Channel(), item.Snippet.PublishedAt, watchURL(item.VideoID()),
			); err != nil {
				return 0, err
			}
		}
		rows.Flush()
		if err := rows.Error(); err != nil {
			return 0, err
		}
	} else {
		for _, item := range entry.Items {
			link := watchURL(item.VideoID())
			if link == "" {
				skipped++
			} else {
				data.WriteString(link + "\n")
			}
		}
	}
	return skipped, fsutil.AtomicWrite(path, data.Bytes(), 0o600)
}

func watchURL(id string) string {
	if videoid.ValidVideo(id) {
		return "https://www.youtube.com/watch?v=" + id
	}
	return ""
}
