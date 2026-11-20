// Package store holds snapshots of playlists and their optional item lists.
package store

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/WyattJia/youtube-playlist-cli/internal/fsutil"
	"github.com/WyattJia/youtube-playlist-cli/internal/videoid"
	"github.com/WyattJia/youtube-playlist-cli/internal/youtube"
)

var fileID = base32.StdEncoding.WithPadding(base32.NoPadding)

var ErrNotCached = errors.New("本地没有此歌单，请运行 ytpl sync --playlist 或使用 --remote")

type Store struct {
	dir string
}

func New(stateDir string) *Store {
	return &Store{dir: filepath.Join(stateDir, "cache", "playlists")}
}

type Entry struct {
	Playlist youtube.Playlist       `json:"playlist"`
	HasItems bool                   `json:"hasItems"`
	Items    []youtube.PlaylistItem `json:"items"`
}

func (s *Store) Get(id string) (Entry, error) {
	if !videoid.ValidPlaylist(id) {
		return Entry{}, videoid.ErrPlaylist
	}
	data, err := os.ReadFile(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return Entry{}, ErrNotCached
	}
	if err != nil {
		return Entry{}, fmt.Errorf("读取歌单缓存: %w", err)
	}
	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		return Entry{}, fmt.Errorf("解析歌单缓存 %s: %w", id, err)
	}
	if entry.Playlist.ID != id || (entry.HasItems && entry.Items == nil) {
		return Entry{}, fmt.Errorf("歌单缓存 %s 内容不完整，请重新同步", id)
	}
	return entry, nil
}

func (s *Store) List() ([]Entry, error) {
	files, err := os.ReadDir(s.dir)
	entries := make([]Entry, 0, len(files))
	if errors.Is(err, fs.ErrNotExist) {
		return entries, nil
	}
	if err != nil {
		return nil, fmt.Errorf("列出歌单缓存: %w", err)
	}
	for _, file := range files {
		name, ok := strings.CutSuffix(file.Name(), ".json")
		if file.IsDir() || !ok {
			continue
		}
		decoded, err := fileID.DecodeString(name)
		if err != nil || !videoid.ValidPlaylist(string(decoded)) || fileID.EncodeToString(decoded) != name {
			continue
		}
		entry, err := s.Get(string(decoded))
		if errors.Is(err, ErrNotCached) {
			continue // A concurrent delete can remove a directory entry.
		}
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b Entry) int {
		if byTitle := strings.Compare(a.Playlist.Snippet.Title, b.Playlist.Snippet.Title); byTitle != 0 {
			return byTitle
		}
		return strings.Compare(a.Playlist.ID, b.Playlist.ID)
	})
	return entries, nil
}

// Save updates metadata. Nil items preserve an existing item snapshot, whereas
// an empty, non-nil list records a playlist that was read and has no items.
func (s *Store) Save(p youtube.Playlist, items []youtube.PlaylistItem) error {
	if !videoid.ValidPlaylist(p.ID) {
		return videoid.ErrPlaylist
	}
	return s.withLock(func() error {
		entry := Entry{Playlist: p, HasItems: items != nil, Items: items}
		if items == nil {
			old, err := s.Get(p.ID)
			if err == nil && old.Playlist.ContentDetails.ItemCount == p.ContentDetails.ItemCount {
				entry.HasItems, entry.Items = old.HasItems, old.Items
			}
			if err != nil && !errors.Is(err, ErrNotCached) {
				return err
			}
		}
		return fsutil.WriteJSON(s.path(p.ID), entry, 0o600)
	})
}

func (s *Store) InvalidateItems(id string) error {
	if !videoid.ValidPlaylist(id) {
		return videoid.ErrPlaylist
	}
	return s.withLock(func() error {
		entry, err := s.Get(id)
		if errors.Is(err, ErrNotCached) {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.HasItems {
			return nil
		}
		entry.HasItems, entry.Items = false, nil
		return fsutil.WriteJSON(s.path(id), entry, 0o600)
	})
}

func (s *Store) Delete(id string) error {
	if !videoid.ValidPlaylist(id) {
		return videoid.ErrPlaylist
	}
	return s.withLock(func() error {
		if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("移除歌单缓存: %w", err)
		}
		return nil
	})
}

func (s *Store) path(id string) string {
	// YouTube IDs are case-sensitive even when the filesystem is not.
	return filepath.Join(s.dir, fileID.EncodeToString([]byte(id))+".json")
}

func (s *Store) withLock(fn func() error) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("创建歌单缓存目录: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return fsutil.WithFileLockContext(ctx, filepath.Join(s.dir, "cache.lock"), fn)
}
