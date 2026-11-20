// Package playlist implements playlist operations shared by the CLI and cache.
package playlist

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/WyattJia/youtube-playlist-cli/internal/store"
	"github.com/WyattJia/youtube-playlist-cli/internal/youtube"
)

type Service struct {
	API   *youtube.Client
	Cache *store.Store
}

// Sync of a single playlist always includes its items. With no playlist ID,
// sync enumerates the current OAuth channel's playlists.
func (s Service) Sync(ctx context.Context, id string, withItems bool) ([]youtube.Playlist, error) {
	var playlists []youtube.Playlist
	if id != "" {
		p, err := s.API.GetPlaylist(ctx, id)
		if err != nil {
			return nil, err
		}
		playlists = []youtube.Playlist{p}
		withItems = true
	} else {
		var err error
		playlists, err = s.API.ListMyPlaylists(ctx)
		if err != nil {
			return nil, err
		}
	}
	for i, p := range playlists {
		var items []youtube.PlaylistItem
		if withItems {
			var err error
			items, err = s.readItems(ctx, p.ID)
			if err != nil {
				return playlists[:i], err
			}
		}
		if err := s.Cache.Save(p, items); err != nil {
			return playlists[:i], err
		}
	}
	return playlists, nil
}

func (s Service) List(ctx context.Context, remote bool) ([]youtube.Playlist, error) {
	if remote {
		return s.Sync(ctx, "", false)
	}
	entries, err := s.Cache.List()
	if err != nil {
		return nil, err
	}
	playlists := make([]youtube.Playlist, 0, len(entries))
	for _, entry := range entries {
		playlists = append(playlists, entry.Playlist)
	}
	return playlists, nil
}

func (s Service) Show(ctx context.Context, id string, remote bool) (store.Entry, error) {
	if remote {
		p, err := s.API.GetPlaylist(ctx, id)
		if err != nil {
			return store.Entry{}, err
		}
		items, err := s.readItems(ctx, id)
		if err != nil {
			return store.Entry{}, err
		}
		entry := store.Entry{Playlist: p, HasItems: true, Items: items}
		return entry, s.Cache.Save(p, items)
	}
	return s.Cache.Get(id)
}

type EditResult struct {
	DryRun   bool             `json:"dryRun"`
	Playlist youtube.Playlist `json:"playlist"`
	Parts    []string         `json:"parts,omitempty"`
	Applied  bool             `json:"applied"`
}

func (s Service) Create(ctx context.Context, title, description, privacy string, dryRun bool) (EditResult, error) {
	if strings.TrimSpace(title) == "" {
		return EditResult{}, errors.New("--title 不能为空")
	}
	if privacy == "" {
		privacy = "private"
	}
	if err := validatePrivacy(privacy); err != nil {
		return EditResult{}, err
	}
	result := EditResult{DryRun: dryRun, Parts: []string{"snippet", "status"}}
	result.Playlist.Snippet.Title, result.Playlist.Snippet.Description = title, description
	result.Playlist.Status.PrivacyStatus = privacy
	if dryRun {
		return result, nil
	}
	p, err := s.API.CreatePlaylist(ctx, title, description, privacy)
	if err != nil {
		return result, err
	}
	result.Playlist, result.Applied = p, true
	return result, s.Cache.Save(p, []youtube.PlaylistItem{})
}

func (s Service) Update(ctx context.Context, id string, patch youtube.PlaylistPatch, dryRun bool) (EditResult, error) {
	if patch.Title == nil && patch.Description == nil && patch.Privacy == nil {
		return EditResult{}, errors.New("至少提供 --title、--description 或 --privacy 之一")
	}
	if patch.Title != nil && strings.TrimSpace(*patch.Title) == "" {
		return EditResult{}, errors.New("--title 不能为空")
	}
	if patch.Privacy != nil {
		if err := validatePrivacy(*patch.Privacy); err != nil {
			return EditResult{}, err
		}
	}
	current, err := s.API.GetPlaylist(ctx, id)
	if err != nil {
		return EditResult{}, err
	}
	next, parts := patch.Apply(current)
	result := EditResult{DryRun: dryRun, Playlist: next, Parts: parts}
	if dryRun || len(parts) == 0 {
		return result, nil
	}
	next, err = s.API.UpdatePlaylist(ctx, current, patch)
	if err != nil {
		return result, err
	}
	result.Playlist, result.Applied = next, true
	return result, s.Cache.Save(next, nil)
}

func (s Service) Delete(ctx context.Context, id string, confirmed, dryRun bool) (EditResult, error) {
	result := EditResult{DryRun: dryRun, Playlist: youtube.Playlist{ID: id}}
	switch {
	case dryRun:
		return result, nil
	case !confirmed:
		return result, errors.New("删除歌单需要 --yes，或使用 --dry-run 查看计划")
	}
	if err := s.API.DeletePlaylist(ctx, id); err != nil {
		return result, err
	}
	result.Applied = true
	return result, s.Cache.Delete(id)
}

func validatePrivacy(privacy string) error {
	switch privacy {
	case "private", "unlisted", "public":
		return nil
	default:
		return errors.New("--privacy 必须是 private、unlisted 或 public")
	}
}

func (s Service) readItems(ctx context.Context, id string) ([]youtube.PlaylistItem, error) {
	items, err := s.API.ListPlaylistItems(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("读取歌单 %s 的条目: %w", id, err)
	}
	slices.SortStableFunc(items, func(a, b youtube.PlaylistItem) int {
		return cmp.Compare(a.Snippet.Position, b.Snippet.Position)
	})
	return items, nil
}
