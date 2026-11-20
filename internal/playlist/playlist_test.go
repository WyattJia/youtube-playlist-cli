package playlist

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/WyattJia/youtube-playlist-cli/internal/store"
	"github.com/WyattJia/youtube-playlist-cli/internal/youtube"
)

const (
	v1 = "aaaaaaaaaaa"
	v2 = "bbbbbbbbbbb"
	v3 = "ccccccccccc"
	v4 = "ddddddddddd"
)

func makeItem(id, vid, name string, pos int) youtube.PlaylistItem {
	item := youtube.PlaylistItem{ID: id}
	item.Snippet.Position = pos
	item.Snippet.Title = name
	item.ContentDetails.VideoID = vid
	return item
}

func TestExportLongMultibyteNameAndRFC4180(t *testing.T) {
	const id = "PLExport"
	p := youtube.Playlist{ID: id}
	p.Snippet.Title = "  ..\"歌单/" + strings.Repeat("长标题🎶", 100) + "\\end:  "
	p.ContentDetails.ItemCount = 2
	items := []youtube.PlaylistItem{
		makeItem("item1", v1, "曲名,\"引号\"\n第二行", 0),
		makeItem("item2", "", "已删除视频", 1),
	}
	s := Service{Cache: store.New(t.TempDir())}
	if err := s.Cache.Save(p, items); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	for _, format := range []string{"csv", "txt", "json"} {
		files, err := s.Export(t.Context(), id, out, format, false)
		if err != nil || len(files) != 1 {
			t.Fatalf("export %s = %+v, %v", format, files, err)
		}
		path := files[0].Path
		name := filepath.Base(path)
		if len(name) > 200 || !utf8.ValidString(name) || filepath.Dir(path) != out || filepath.Ext(path) != "."+format ||
			!strings.HasPrefix(name, "_歌单_长标题") {
			t.Fatalf("invalid filename %q (%d bytes)", name, len(name))
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		switch format {
		case "csv":
			if !strings.Contains(string(data), "\r\n") {
				t.Fatal("expected RFC-4180 CRLF")
			}
			rows, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
			if err != nil || len(rows) != 3 || rows[1][3] != items[0].Snippet.Title || rows[1][6] != watchURL(v1) {
				t.Fatalf("CSV rows = %+v, %v", rows, err)
			}
		case "txt":
			if string(data) != watchURL(v1)+"\n" || files[0].Skipped != 1 {
				t.Fatalf("TXT export = %q, %+v", data, files[0])
			}
		case "json":
			var exported store.Entry
			if err := json.Unmarshal(data, &exported); err != nil || len(exported.Items) != 2 ||
				exported.Playlist.Snippet.Title != p.Snippet.Title {
				t.Fatalf("JSON export = %+v, %v", exported, err)
			}
		}
	}
}

func TestExportNeutralizesCSVFormulasAndPreservesRawJSON(t *testing.T) {
	const id = "PLExport"
	cases := []struct {
		raw string
		csv string
	}{
		{"=1+1", "'=1+1"},
		{"  +1", "'  +1"},
		{"-1", "'-1"},
		{"\u00a0@SUM(1,2)", "'\u00a0@SUM(1,2)"},
		{"\ttext", "'\ttext"},
		{"\rtext", "'text"}, // The CRLF-mode CSV writer removes bare CRs.
		{"normal", "normal"},
	}
	items := make([]youtube.PlaylistItem, 0, len(cases))
	for i, test := range cases {
		items = append(items, makeItem(fmt.Sprintf("item%d", i), v1, test.raw, i))
	}
	s := Service{Cache: store.New(t.TempDir())}
	if err := s.Cache.Save(youtube.Playlist{ID: id}, items); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	for _, format := range []string{"csv", "json"} {
		files, err := s.Export(t.Context(), id, out, format, false)
		if err != nil || len(files) != 1 {
			t.Fatalf("export %s = %+v, %v", format, files, err)
		}
		data, err := os.ReadFile(files[0].Path)
		if err != nil {
			t.Fatal(err)
		}
		if format == "csv" {
			rows, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
			if err != nil || len(rows) != len(cases)+1 {
				t.Fatalf("CSV rows = %+v, %v", rows, err)
			}
			for i, test := range cases {
				if got := rows[i+1][3]; got != test.csv {
					t.Errorf("CSV title %q, want %q", got, test.csv)
				}
			}
		} else {
			var exported store.Entry
			if err := json.Unmarshal(data, &exported); err != nil || len(exported.Items) != len(cases) {
				t.Fatalf("JSON export = %+v, %v", exported, err)
			}
			for i, test := range cases {
				if got := exported.Items[i].Snippet.Title; got != test.raw {
					t.Errorf("JSON title %q, want %q", got, test.raw)
				}
			}
		}
	}
}

type credentials struct{}

func (credentials) AccessToken(context.Context, string) (string, error) {
	return "synthetic-access", nil
}

func TestDeletePlaylistKeepsCacheOnImmediateNotFoundAndClearsItAfterAmbiguity(t *testing.T) {
	const id = "PLDelete"
	for _, test := range []struct {
		name      string
		ambiguous bool
		requests  int32
	}{
		{"immediate 404", false, 1},
		{"lost response then 404", true, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete || r.URL.Path != "/playlists" || r.URL.Query().Get("id") != id {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				if requests.Add(1) == 1 && test.ambiguous {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					if err := conn.Close(); err != nil {
						t.Error(err)
					}
					return
				}
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"error":{"errors":[{"reason":"playlistNotFound"}]}}`)
			}))
			t.Cleanup(server.Close)
			s := Service{
				API: youtube.New(youtube.Options{
					BaseURL: server.URL, HTTPClient: server.Client(), Tokens: credentials{},
					Wait: func(context.Context, time.Duration) error { return nil },
				}),
				Cache: store.New(t.TempDir()),
			}
			if err := s.Cache.Save(youtube.Playlist{ID: id}, []youtube.PlaylistItem{makeItem("item1", v1, "Cached", 0)}); err != nil {
				t.Fatal(err)
			}
			result, err := s.Delete(t.Context(), id, true, false)
			entry, cacheErr := s.Cache.Get(id)
			if test.ambiguous {
				if err != nil || !result.Applied || !errors.Is(cacheErr, store.ErrNotCached) {
					t.Fatalf("recovered delete = %+v, %v, cache error %v", result, err, cacheErr)
				}
			} else {
				if youtube.KindOf(err) != youtube.PlaylistNotFound || result.Applied || cacheErr != nil || !entry.HasItems || len(entry.Items) != 1 {
					t.Fatalf("immediate 404 = %+v, %v, cache %+v, %v", result, err, entry, cacheErr)
				}
			}
			if got := requests.Load(); got != test.requests {
				t.Fatalf("delete requests = %d, want %d", got, test.requests)
			}
		})
	}
}

func TestAddRemoveDedupeAndReorderAgainstRemote(t *testing.T) {
	const id = "PLManaged"
	var mu sync.Mutex
	items := []youtube.PlaylistItem{makeItem("item1", v1, "First", 0), makeItem("item2", v2, "Second", 1)}
	updates, deletes, inserts := 0, 0, 0
	deletedOnce := make(map[string]bool)
	type itemPayload struct {
		ID      string `json:"id"`
		Snippet struct {
			PlaylistID string             `json:"playlistId"`
			Position   *int               `json:"position"`
			ResourceID youtube.ResourceID `json:"resourceId"`
		} `json:"snippet"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/playlistItems" || r.Header.Get("Authorization") != "Bearer synthetic-access" {
			t.Error("unexpected API path or credentials")
			http.Error(w, "bad request", 400)
			return
		}
		respond := func(value any) {
			if err := json.NewEncoder(w).Encode(value); err != nil {
				t.Error(err)
			}
		}
		fixPositions := func() {
			for i := range items {
				items[i].Snippet.Position = i
			}
		}
		switch r.Method {
		case "GET":
			if r.URL.Query().Get("playlistId") != id {
				t.Error("listed wrong playlist")
			}
			respond(map[string]any{"items": items})
		case "POST", "PUT":
			var body itemPayload
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Snippet.PlaylistID != id || body.Snippet.ResourceID.Kind != "youtube#video" {
				t.Errorf("invalid item payload: %v", err)
				http.Error(w, "bad payload", 400)
				return
			}
			if r.Method == "POST" {
				inserts++
				position := len(items)
				if body.Snippet.Position != nil {
					position = *body.Snippet.Position
				}
				item := makeItem(fmt.Sprintf("added%d", inserts), body.Snippet.ResourceID.VideoID, "Added", position)
				items = slices.Insert(items, position, item)
				fixPositions()
				respond(item)
			} else {
				updates++
				idx := slices.IndexFunc(items, func(item youtube.PlaylistItem) bool { return item.ID == body.ID })
				if idx < 0 || body.Snippet.Position == nil || items[idx].VideoID() != body.Snippet.ResourceID.VideoID {
					t.Error("invalid move")
					http.Error(w, "invalid move", 400)
					return
				}
				item := items[idx]
				items = slices.Delete(items, idx, idx+1)
				items = slices.Insert(items, *body.Snippet.Position, item)
				fixPositions()
				respond(item)
			}
		case "DELETE":
			deletes++
			itemID := r.URL.Query().Get("id")
			idx := slices.IndexFunc(items, func(item youtube.PlaylistItem) bool { return item.ID == itemID })
			if idx < 0 {
				w.WriteHeader(404)
				respond(map[string]any{"error": map[string]any{"errors": []map[string]string{{"reason": "playlistItemNotFound"}}}})
				return
			}
			items = slices.Delete(items, idx, idx+1)
			fixPositions()
			if itemID == "added3" && !deletedOnce[itemID] {
				deletedOnce[itemID] = true
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				if err := conn.Close(); err != nil {
					t.Error(err)
				}
				return
			}
			w.WriteHeader(204)
		default:
			t.Error("unexpected HTTP method")
		}
	}))
	defer server.Close()
	client := youtube.New(youtube.Options{
		BaseURL: server.URL, HTTPClient: server.Client(), Tokens: credentials{},
		Wait: func(context.Context, time.Duration) error { return nil },
	})
	s := Service{API: client, Cache: store.New(t.TempDir())}
	position := 1
	withDedup, err := s.Add(t.Context(), id, []string{v1, v3, v3, v4}, false, &position, false)
	if err != nil || withDedup.Applied != 2 || withDedup.Skipped != 2 ||
		*withDedup.Changes[0].Position != 1 || *withDedup.Changes[1].Position != 2 {
		t.Fatalf("add = %+v, %v", withDedup, err)
	}
	duplicates, err := s.Add(t.Context(), id, []string{v3}, true, nil, false)
	if err != nil || duplicates.Applied != 1 {
		t.Fatalf("allow duplicate = %+v, %v", duplicates, err)
	}
	// Explicit item IDs must belong to the playlist named on the command line.
	if _, err := s.Remove(t.Context(), id, []string{"fromOtherPlaylist"}, nil, false, false); youtube.KindOf(err) != youtube.ItemNotFound {
		t.Fatalf("cross-playlist removal = %v", err)
	}
	deduped, err := s.Dedupe(t.Context(), id, "first", false)
	if err != nil || deduped.Applied != 1 || deduped.Changes[0].ItemID != "added3" {
		t.Fatalf("dedupe with lost DELETE response = %+v, %v", deduped, err)
	}
	moved, err := s.Reorder(t.Context(), id, Order{From: 1, To: 3}, false)
	if err != nil || moved.Applied != 1 || moved.Changes[0].ItemID != "item1" || *moved.Changes[0].Position != 2 {
		t.Fatalf("manual move = %+v, %v", moved, err)
	}
	removed, err := s.Remove(t.Context(), id, nil, []string{v4}, false, false)
	if err != nil || removed.Applied != 1 || removed.Changes[0].ItemID != "added2" {
		t.Fatalf("remove by video = %+v, %v", removed, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if inserts != 3 || updates != 1 || deletes != 3 || len(items) != 3 ||
		items[0].VideoID() != v3 || items[1].VideoID() != v1 || items[2].VideoID() != v2 {
		t.Fatalf("remote state inserts=%d updates=%d deletes=%d items=%+v", inserts, updates, deletes, items)
	}
}
