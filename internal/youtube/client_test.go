package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/WyattJia/youtube-playlist-cli/internal/quota"
)

type tokenFunc func(context.Context, string) (string, error)

func (f tokenFunc) AccessToken(ctx context.Context, rejected string) (string, error) {
	return f(ctx, rejected)
}

func noWait(context.Context, time.Duration) error { return nil }

func mockClient(t *testing.T, handler http.HandlerFunc, opts Options) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	opts.BaseURL, opts.HTTPClient = server.URL, server.Client()
	if opts.Wait == nil {
		opts.Wait = noWait
	}
	return New(opts)
}

func staticToken(context.Context, string) (string, error) { return "synthetic-oauth", nil }

func respondJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func respondError(t *testing.T, w http.ResponseWriter, status int, reason string) {
	t.Helper()
	w.WriteHeader(status)
	respondJSON(t, w, map[string]any{"error": map[string]any{
		"errors": []map[string]string{{"reason": reason}}, "message": "synthetic server diagnostic",
	}})
}

func TestPaginationChunkingAndCredentials(t *testing.T) {
	const key = "synthetic-public+key"
	var reads atomic.Int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		q := r.URL.Query()
		var items []map[string]string
		next := ""
		switch r.URL.Path {
		case "/playlists":
			if q.Get("part") != "snippet,status,contentDetails" {
				t.Errorf("unexpected playlist part: %q", q.Get("part"))
			}
			if q.Get("mine") == "true" {
				if q.Get("key") != "" || r.Header.Get("Authorization") != "Bearer synthetic-oauth" || q.Get("maxResults") != "50" {
					t.Error("invalid playlists.list mine authentication or page size")
				}
				if q.Get("pageToken") == "" {
					items, next = []map[string]string{{"id": "PLFirst"}}, "page-token"
				} else {
					items = []map[string]string{{"id": "PLSecond"}}
				}
			} else {
				if q.Get("id") != "PLPublic" || q.Get("key") != key || r.Header.Get("Authorization") != "" {
					t.Error("invalid get playlist authentication or selection")
				}
				items = []map[string]string{{"id": "PLPublic"}}
			}
		case "/playlistItems":
			if q.Get("part") != "snippet,contentDetails,status" || q.Get("maxResults") != "50" ||
				q.Get("playlistId") != "PLPublic" || q.Get("key") != key || r.Header.Get("Authorization") != "" {
				t.Error("invalid playlist item request")
			}
			if q.Get("pageToken") == "" {
				items, next = []map[string]string{{"id": "item1"}}, "items-2"
			} else {
				items = []map[string]string{{"id": "item2"}}
			}
		case "/videos":
			if q.Get("part") != "snippet,contentDetails,status" || q.Get("key") != key || r.Header.Get("Authorization") != "" {
				t.Error("invalid videos.list request")
			}
			ids := strings.Split(q.Get("id"), ",")
			if ids[0] == "vid00000000" && len(ids) != 50 || ids[0] == "vid00000050" && len(ids) != 1 {
				t.Errorf("invalid batch length %d", len(ids))
			}
			for _, id := range ids {
				items = append(items, map[string]string{"id": id})
			}
		default:
			t.Error("unexpected endpoint")
			http.Error(w, "unexpected", 400)
			return
		}
		respondJSON(t, w, map[string]any{"items": items, "nextPageToken": next})
	}
	server := httptest.NewServer(http.HandlerFunc(handler))
	defer server.Close()

	getKey := func() (string, error) { return key, nil }
	signed := New(Options{BaseURL: server.URL, HTTPClient: server.Client(), Tokens: tokenFunc(staticToken), APIKey: getKey})
	public := New(Options{BaseURL: server.URL, HTTPClient: server.Client(), APIKey: getKey})
	mine, err := signed.ListMyPlaylists(t.Context())
	if err != nil || len(mine) != 2 || mine[0].ID != "PLFirst" || mine[1].ID != "PLSecond" {
		t.Fatalf("mine = %+v, %v", mine, err)
	}
	p, err := public.GetPlaylist(t.Context(), "PLPublic")
	if err != nil || p.ID != "PLPublic" {
		t.Fatalf("public playlist = %+v, %v", p, err)
	}
	items, err := public.ListPlaylistItems(t.Context(), p.ID)
	if err != nil || len(items) != 2 || items[0].ID != "item1" || items[1].ID != "item2" {
		t.Fatalf("playlist items = %+v, %v", items, err)
	}
	var ids []string
	for i := range 51 {
		ids = append(ids, fmt.Sprintf("vid%08d", i))
	}
	vids, err := public.ListVideos(t.Context(), ids)
	if err != nil || len(vids) != 51 || vids[50].ID != ids[50] {
		t.Fatalf("video chunks returned %d videos: %v", len(vids), err)
	}
	if _, err := public.ListMyPlaylists(t.Context()); KindOf(err) != AuthRequired {
		t.Fatalf("API key unexpectedly allowed mine: %v", err)
	}
	if _, err := public.CreatePlaylist(t.Context(), "test", "", "private"); KindOf(err) != AuthRequired {
		t.Fatalf("API key unexpectedly allowed a write: %v", err)
	}
	if reads.Load() != 7 {
		t.Fatalf("received %d requests, want 7", reads.Load())
	}
}

func TestUpdateOnlyChangedPartsWithFullReplacement(t *testing.T) {
	p := Playlist{ID: "PLPod"}
	p.Snippet = PlaylistSnippet{Title: "原名", Description: "简介", DefaultLanguage: "zh-CN"}
	p.Status = PlaylistStatus{PrivacyStatus: "private", PodcastStatus: "enabled"}
	var updates []string
	var mu sync.Mutex
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case "GET":
			respondJSON(t, w, map[string]any{"items": []Playlist{p}})
		case "PUT":
			part := r.URL.Query().Get("part")
			updates = append(updates, part)
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				http.Error(w, "bad body", 400)
				return
			}
			if string(body["id"]) != `"PLPod"` || len(body) != 1+len(strings.Split(part, ",")) {
				t.Error("body doesn't match modified parts")
			}
			for _, field := range strings.Split(part, ",") {
				switch field {
				case "snippet":
					p.Snippet = PlaylistSnippet{}
					if err := json.Unmarshal(body[field], &p.Snippet); err != nil {
						t.Error(err)
					}
				case "status":
					p.Status = PlaylistStatus{}
					if err := json.Unmarshal(body[field], &p.Status); err != nil {
						t.Error(err)
					}
				default:
					t.Error("unexpected updated part")
				}
			}
			respondJSON(t, w, p)
		default:
			t.Error("unexpected method")
		}
	}, Options{Tokens: tokenFunc(staticToken)})

	current, err := client.GetPlaylist(t.Context(), "PLPod")
	if err != nil {
		t.Fatal(err)
	}
	title, privacy, description := "新标题", "unlisted", ""
	for _, patch := range []PlaylistPatch{
		{Title: &title}, {Privacy: &privacy}, {Privacy: &privacy}, {Description: &description},
	} {
		current, err = client.UpdatePlaylist(t.Context(), current, patch)
		if err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(updates, []string{"snippet", "status", "snippet"}) {
		t.Fatalf("updated parts = %v", updates)
	}
	if p.Status.PodcastStatus != "enabled" || p.Status.PrivacyStatus != privacy ||
		p.Snippet.Title != title || p.Snippet.DefaultLanguage != "zh-CN" || p.Snippet.Description != "" {
		t.Fatalf("part replacement lost fields: %+v", p)
	}
}

func TestTransientRetriesAndHTML403(t *testing.T) {
	fixed := time.Date(2026, 3, 8, 10, 0, 0, 0, time.UTC)
	var attempts atomic.Int32
	var waited []time.Duration
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch attempts.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "2")
			http.Error(w, "<html>Bad Gateway</html>", http.StatusBadGateway)
		case 2:
			w.Header().Set("Retry-After", fixed.Add(3*time.Second).Format(http.TimeFormat))
			http.Error(w, "<html>Too Many Requests</html>", http.StatusTooManyRequests)
		case 3:
			fmt.Fprint(w, `{"items":[{"id":"PLPublic"}]}`)
		default:
			http.Error(w, "<html>Forbidden body</html>", http.StatusForbidden)
		}
	}, Options{
		APIKey: func() (string, error) { return "test-key", nil },
		Now:    func() time.Time { return fixed },
		Wait: func(_ context.Context, d time.Duration) error {
			waited = append(waited, d)
			return nil
		},
	})
	if _, err := client.GetPlaylist(t.Context(), "PLPublic"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(waited, []time.Duration{2 * time.Second, 3 * time.Second}) {
		t.Fatalf("waits = %v", waited)
	}
	if _, err := client.GetPlaylist(t.Context(), "PLPublic"); KindOf(err) != PermissionDenied ||
		strings.Contains(err.Error(), "<html>") || attempts.Load() != 4 {
		t.Fatalf("HTML 403 classified or retried incorrectly: attempts %d, error %v", attempts.Load(), err)
	}
	for _, layout := range []string{http.TimeFormat, time.RFC1123, time.RFC850, time.ANSIC} {
		if got := retryDelay(fixed.Add(6*time.Second).Format(layout), fixed); got != 6*time.Second {
			t.Errorf("Retry-After %s: delay = %v", layout, got)
		}
	}
}

func TestErrorClassificationAndRedaction(t *testing.T) {
	for _, test := range []struct {
		status int
		reason string
		kind   ErrorKind
	}{
		{403, "playlistContainsMaximumNumberOfVideos", PlaylistFull},
		{400, "videoAlreadyInAnotherSeriesPlaylist", SeriesConflict},
		{400, "invalidVideoId", ItemNotFound},
		{404, "videoNotFound", ItemNotFound},
		{403, "videoNotAvailable", ItemNotFound},
		{404, "playlistNotFound", PlaylistNotFound},
		{404, "playlistItemNotFound", ItemNotFound},
		{404, "", APIRejected},
		{404, "channelNotFound", ChannelNotFound},
		{409, "rateLimitExceeded", Transient},
		{403, "insufficientPermissions", PermissionDenied},
		{403, "dailyLimitExceeded", QuotaExceeded},
	} {
		t.Run(test.reason, func(t *testing.T) {
			const secret = "secret+api key"
			body, err := json.Marshal(map[string]any{"error": map[string]any{
				"errors":  []map[string]string{{"reason": test.reason}},
				"status":  "PERMISSION_DENIED",
				"message": fmt.Sprintf("GET https://www.googleapis.com/youtube/v3/playlists?key=%s. Authorization: Bearer synthetic-secret", url.QueryEscape(secret)),
			}})
			if err != nil {
				t.Fatal(err)
			}
			apiErr := decodeError(test.status, "", body, time.Now())
			if apiErr.Kind != test.kind || apiErr.Reason != test.reason || apiErr.StatusCode != test.status ||
				apiErr.GoogleStatus != "PERMISSION_DENIED" {
				t.Fatalf("error classification = %+v", apiErr)
			}
			for _, value := range []string{secret, url.QueryEscape(secret), "synthetic-secret", "googleapis.com", "Authorization:"} {
				if strings.Contains(fmt.Sprintf("%+v", apiErr), value) || strings.Contains(apiErr.Message, value) {
					t.Fatal("API error leaked remote credentials")
				}
			}
		})
	}
}

func TestQuotaExceededDoesNotRetryAndPersistsReset(t *testing.T) {
	fixed := time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)
	ledger, err := quota.New(t.TempDir(), func() time.Time { return fixed })
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		respondError(t, w, 403, "quotaExceeded")
	}, Options{Tokens: tokenFunc(staticToken), Meter: &quota.Meter{Ledger: ledger}})
	_, err = client.ListMyPlaylists(t.Context())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != QuotaExceeded || !apiErr.ResetAt.Equal(fixed.Add(25*time.Hour)) {
		t.Fatalf("quota error = %v", err)
	}
	if _, err := client.ListMyPlaylists(t.Context()); KindOf(err) != QuotaExceeded || calls.Load() != 1 {
		t.Fatalf("exhausted state didn't block next request: %v (request count %d)", err, calls.Load())
	}
	s, err := ledger.Read()
	if err != nil || !s.Exhausted || s.Used != 1 {
		t.Fatalf("ledger = %+v, %v", s, err)
	}
}

func TestUnauthorizedRefreshesOnlyOnce(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprintf("persistent=%t", persistent), func(t *testing.T) {
			refreshes := 0
			var requests atomic.Int32
			client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				n := requests.Add(1)
				auth := r.Header.Get("Authorization")
				if n == 1 && auth != "Bearer old" || n == 2 && auth != "Bearer next" {
					t.Error("sent wrong OAuth token")
				}
				if n == 1 || persistent {
					respondError(t, w, 401, "authError")
				} else {
					fmt.Fprint(w, `{"items":[{"id":"UCTest"}]}`)
				}
			}, Options{Tokens: tokenFunc(func(_ context.Context, rejected string) (string, error) {
				if rejected == "" {
					return "old", nil
				}
				refreshes++
				if rejected != "old" {
					t.Error("forced refresh did not receive rejected token")
				}
				return "next", nil
			})})
			_, err := client.GetMyChannel(t.Context())
			if refreshes != 1 || requests.Load() != 2 || (!persistent && err != nil) ||
				(persistent && KindOf(err) != AuthExpired) {
				t.Fatalf("refreshes=%d requests=%d err=%v", refreshes, requests.Load(), err)
			}
		})
	}
}

func TestDeleteAmbiguousThenNotFoundAndImmediateNotFound(t *testing.T) {
	for _, test := range []struct {
		first string
		want  ErrorKind
	}{
		{"drop", ""}, {"502", ""}, {"404", ItemNotFound}, {"429", ItemNotFound},
	} {
		t.Run(test.first, func(t *testing.T) {
			var requests atomic.Int32
			client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "DELETE" || r.URL.Path != "/playlistItems" || r.URL.Query().Get("id") != "item1" {
					t.Error("unexpected delete request")
				}
				if requests.Add(1) == 1 {
					switch test.first {
					case "drop":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						if err := conn.Close(); err != nil {
							t.Error(err)
						}
						return
					case "502":
						http.Error(w, "<html>lost response</html>", 502)
						return
					case "429":
						respondError(t, w, 429, "rateLimitExceeded")
						return
					}
				}
				respondError(t, w, 404, "playlistItemNotFound")
			}, Options{Tokens: tokenFunc(staticToken)})
			err := client.DeletePlaylistItem(t.Context(), "item1")
			if KindOf(err) != test.want || (test.want == "" && err != nil) {
				t.Fatalf("delete = %v, want %s", err, test.want)
			}
			wantRequests := int32(2)
			if test.first == "404" {
				wantRequests = 1
			}
			if requests.Load() != wantRequests {
				t.Fatalf("requests = %d, want %d", requests.Load(), wantRequests)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportErrorsAreRedactedAndAmbiguousPostIsNotRetried(t *testing.T) {
	const key = "secret-key+value"
	attempts := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		return nil, fmt.Errorf("transport diagnostic %s Authorization: Bearer bearer-private", r.URL.String())
	})}
	opts := Options{
		HTTPClient: httpClient, BaseURL: "https://local-test.invalid", Wait: noWait,
		APIKey: func() (string, error) { return key, nil },
	}
	_, err := New(opts).GetPlaylist(t.Context(), "PLPublic")
	if KindOf(err) != Transient || attempts != 4 || strings.Contains(err.Error(), url.QueryEscape(key)) ||
		strings.Contains(err.Error(), "bearer-private") || strings.Contains(err.Error(), "local-test.invalid") {
		t.Fatalf("transport error not handled safely: attempts %d, error %v", attempts, err)
	}
	opts.Tokens = tokenFunc(staticToken)
	attempts = 0
	_, err = New(opts).CreatePlaylist(t.Context(), "new", "", "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.Ambiguous || attempts != 1 {
		t.Fatalf("ambiguous create dispatched %d times: %v", attempts, err)
	}
}

func TestRedirectDoesNotSendCredentialsToAnotherOrigin(t *testing.T) {
	var reached atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Store(true)
		if _, err := io.WriteString(w, `{"items":[]}`); err != nil {
			t.Error(err)
		}
	}))
	defer other.Close()
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), 302)
	}, Options{APIKey: func() (string, error) { return "test-key", nil }})
	if _, err := client.GetPlaylist(t.Context(), "PLPublic"); err == nil || reached.Load() {
		t.Fatalf("redirect was unexpectedly followed: %v", err)
	}
}

func TestConflictAbortedRetriesOnInsert(t *testing.T) {
	var attempts atomic.Int32
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":{"code":409,"status":"ABORTED","errors":[{"reason":"SERVICE_UNAVAILABLE"}]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"item1","snippet":{"resourceId":{"videoId":"aaaaaaaaaaa"}}}`)
	}, Options{Tokens: tokenFunc(staticToken)})
	item, err := client.InsertPlaylistItem(t.Context(), "PLTest", "aaaaaaaaaaa", nil)
	if err != nil || item.ID != "item1" || attempts.Load() != 2 {
		t.Fatalf("409 ABORTED retry = %+v, attempts=%d, err=%v", item, attempts.Load(), err)
	}
}
