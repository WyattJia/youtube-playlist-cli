package store

import (
	"errors"
	"testing"

	"github.com/WyattJia/youtube-playlist-cli/internal/youtube"
)

func TestCacheMetadataAndItemSnapshots(t *testing.T) {
	s := New(t.TempDir())
	p := youtube.Playlist{ID: "PL_test"}
	p.Snippet.Title = "原始标题"
	p.ContentDetails.ItemCount = 1
	if _, err := s.Get(p.ID); !errors.Is(err, ErrNotCached) {
		t.Fatalf("missing playlist returned %v", err)
	}
	if err := s.Save(p, []youtube.PlaylistItem{{ID: "entry1"}}); err != nil {
		t.Fatal(err)
	}
	p.Snippet.Title = "新标题"
	if err := s.Save(p, nil); err != nil {
		t.Fatal(err)
	}
	entry, err := s.Get(p.ID)
	if err != nil || entry.Playlist.Snippet.Title != "新标题" || !entry.HasItems || len(entry.Items) != 1 {
		t.Fatalf("metadata refresh lost item cache: %+v, %v", entry, err)
	}
	p.ContentDetails.ItemCount = 0
	if err := s.Save(p, nil); err != nil {
		t.Fatal(err)
	}
	entry, err = s.Get(p.ID)
	if err != nil || entry.HasItems {
		t.Fatalf("stale items were kept: %+v, %v", entry, err)
	}
	if err := s.Save(p, []youtube.PlaylistItem{}); err != nil {
		t.Fatal(err)
	}
	entry, err = s.Get(p.ID)
	if err != nil || !entry.HasItems || entry.Items == nil || len(entry.Items) != 0 {
		t.Fatalf("empty snapshot = %+v, %v", entry, err)
	}
	if err := s.InvalidateItems(p.ID); err != nil {
		t.Fatal(err)
	}
	entries, err := s.List()
	if err != nil || len(entries) != 1 || entries[0].HasItems {
		t.Fatalf("list = %+v, %v", entries, err)
	}
	if err := s.Delete(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(p.ID); !errors.Is(err, ErrNotCached) {
		t.Fatalf("delete left entry: %v", err)
	}
	for _, id := range []string{"../secret", "PL/a"} {
		if err := s.Save(youtube.Playlist{ID: id}, nil); err == nil {
			t.Fatalf("cache accepted %q", id)
		}
	}
}

func TestCaseSensitiveYouTubeIDsOnAnyFilesystem(t *testing.T) {
	s := New(t.TempDir())
	for _, id := range []string{"PLMixedCase", "PLmixedcase"} {
		if err := s.Save(youtube.Playlist{ID: id}, []youtube.PlaylistItem{{ID: "item-" + id}}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.List()
	if err != nil || len(entries) != 2 {
		t.Fatalf("case variants were not cached separately: %+v, %v", entries, err)
	}
	for _, entry := range entries {
		if entry.Items[0].ID != "item-"+entry.Playlist.ID {
			t.Fatalf("cache resolved distinct IDs to the wrong item list: %+v", entry)
		}
	}
}
