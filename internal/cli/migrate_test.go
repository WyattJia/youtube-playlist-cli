package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WyattJia/youtube-playlist-cli/internal/queue"
	"github.com/WyattJia/youtube-playlist-cli/internal/youtube"
)

const migrateFixture = `{"sourceCommit":"abc123456789","playlists":[{
  "neteasePlaylistId":"1","name":"演示歌单","youtubePlaylistId":%s,
  "tracks":[{"neteaseSongId":"11","videoId":"aaaaaaaaaaa"},{"neteaseSongId":"12","videoId":"bbbbbbbbbbb"}]
}]}`

type postedPlaylist struct {
	Snippet struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	} `json:"snippet"`
	Status struct {
		Privacy string `json:"privacyStatus"`
	} `json:"status"`
}

func insertVideo(t *testing.T, w http.ResponseWriter, r *http.Request, target string) string {
	t.Helper()
	var body struct {
		Snippet struct {
			ID       string `json:"playlistId"`
			Resource struct {
				VideoID string `json:"videoId"`
			} `json:"resourceId"`
		} `json:"snippet"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Snippet.ID != target || body.Snippet.Resource.VideoID == "" {
		t.Error("invalid remote item insert")
		http.Error(w, "invalid", 400)
		return ""
	}
	return body.Snippet.Resource.VideoID
}

func writeItems(t *testing.T, w http.ResponseWriter, videos []string) {
	t.Helper()
	items := make([]any, 0, len(videos))
	for i, video := range videos {
		items = append(items, map[string]any{
			"id": fmt.Sprintf("item%d", i), "contentDetails": map[string]string{"videoId": video},
		})
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"items": items}); err != nil {
		t.Error(err)
	}
}

func TestMigrationBudgetWriteLimitQuotaExhaustionAndReserve(t *testing.T) {
	var mu sync.Mutex
	var remote []string
	failQuota := false
	readCount, writeCount := 0, 0
	h := harness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/youtube/v3/playlistItems" || r.Header.Get("Authorization") != "Bearer cli-secret-access" {
			t.Error("unexpected batch request")
			http.Error(w, "invalid", 400)
			return
		}
		switch r.Method {
		case "GET":
			readCount++
			writeItems(t, w, remote)
		case "POST":
			writeCount++
			video := insertVideo(t, w, r, "PLTarget")
			if video == "" {
				return
			}
			if failQuota {
				w.WriteHeader(403)
				fmt.Fprint(w, `{"error":{"errors":[{"reason":"quotaExceeded"}],"message":"Authorization: Bearer cli-secret-access"}}`)
				return
			}
			remote = append(remote, video)
			fmt.Fprintf(w, `{"id":"new%d"}`, writeCount)
		default:
			t.Error("unexpected batch method")
		}
	})
	h.importOAuth()
	plan := h.write("fixture-plan.json", fmt.Sprintf(migrateFixture, `"PLTarget"`))
	decodeResponse[any](t, h.run("", "migrate", "init", "--plan", plan, "--json"), 0)
	run := func(flags ...string) migrationResult {
		t.Helper()
		base := []string{"migrate", "run", "--json"}
		inv := h.run("", append(base, flags...)...)
		assertNoSecrets(t, inv, "cli-secret-access", "Authorization:")
		return decodeResponse[migrationResult](t, inv, 0).Data
	}
	stop := run("--budget", "1")
	if stop.Stopped == nil || stop.Stopped.Code != "BUDGET_EXHAUSTED" || stop.Units != 1 || stop.WriteAttempts != 0 {
		t.Fatalf("budget stop = %+v", stop)
	}
	stop = run("--budget", "1000", "--max-writes", "1")
	if stop.Stopped == nil || stop.Stopped.Code != "MAX_WRITES" || stop.Inserted != 1 ||
		stop.Units != 51 || stop.WriteAttempts != 1 {
		t.Fatalf("write limit stop = %+v", stop)
	}
	mu.Lock()
	failQuota = true
	mu.Unlock()
	stop = run("--budget", "51")
	if stop.Stopped == nil || stop.Stopped.Code != "QUOTA_EXCEEDED" || stop.Stopped.ResetAt == nil ||
		!stop.Stopped.ResetAt.Equal(h.now.Add(25*time.Hour)) || stop.Units != 51 || stop.WriteAttempts != 1 {
		t.Fatalf("Google quota stop = %+v", stop)
	}
	stop = run("--budget", "500")
	if stop.Stopped == nil || stop.Stopped.Code != "QUOTA_EXCEEDED" || stop.Units != 0 {
		t.Fatalf("exhausted ledger should block all requests: %+v", stop)
	}
	q := decodeResponse[map[string]any](t, h.run("", "quota", "--json"), 0)
	if q.Data["used"] != float64(103) || q.Data["exhausted"] != true {
		t.Fatalf("ledger state = %+v", q.Data)
	}
	decodeResponse[any](t, h.run("", "quota", "--reset-exhausted", "--limit", "604", "--json"), 0)
	mu.Lock()
	failQuota = false
	mu.Unlock()
	stop = run()
	if stop.Stopped == nil || stop.Stopped.Code != "QUOTA_EXCEEDED" || stop.Units != 1 || stop.WriteAttempts != 0 {
		t.Fatalf("default reserve stop = %+v", stop)
	}
	complete := run("--budget", "51")
	if complete.Stopped != nil || complete.Inserted != 1 || complete.Playlists[0].Status != "done" {
		t.Fatalf("explicit daily-budget continuation = %+v", complete)
	}
	mu.Lock()
	defer mu.Unlock()
	if readCount != 5 || writeCount != 3 || !slices.Equal(remote, []string{cliVideoA, cliVideoB}) {
		t.Fatalf("remote reads=%d writes=%d tracks=%v", readCount, writeCount, remote)
	}
}

func TestMigrationRecoversActualCreateAndInsertAfterLostHTTPResponses(t *testing.T) {
	var mu sync.Mutex
	var playlist postedPlaylist
	creates, scans, inserts := 0, 0, 0
	var remote []string
	var queueFile string
	h := harness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/youtube/v3/playlists" && r.Method == "POST":
			creates++
			var before struct {
				Playlists []struct {
					Pending bool `json:"creationPending"`
				} `json:"playlists"`
			}
			disk, err := os.ReadFile(queueFile)
			if err != nil || json.Unmarshal(disk, &before) != nil || len(before.Playlists) != 1 || !before.Playlists[0].Pending {
				t.Error("POST happened before durable creationPending")
			}
			if err := json.NewDecoder(r.Body).Decode(&playlist); err != nil ||
				playlist.Snippet.Title != "演示歌单" ||
				playlist.Snippet.Description != "网易云音乐歌单 ID 1 · 备份 abc1234" ||
				playlist.Status.Privacy != "private" {
				t.Error("invalid playlist create payload")
			}
			http.Error(w, "<html>lost outcome</html>", 502)
		case r.URL.Path == "/youtube/v3/playlists" && r.Method == "GET":
			scans++
			if r.URL.Query().Get("mine") != "true" {
				t.Error("recovery did not query owned playlists")
			}
			if scans == 1 {
				fmt.Fprint(w, `{"items":[]}`)
			} else {
				fmt.Fprintf(w, `{"items":[{"id":"PLRecovered","snippet":{"title":%q,"description":%q}}]}`, playlist.Snippet.Title, playlist.Snippet.Description)
			}
		case r.URL.Path == "/youtube/v3/playlistItems" && r.Method == "GET":
			writeItems(t, w, remote)
		case r.URL.Path == "/youtube/v3/playlistItems" && r.Method == "POST":
			video := insertVideo(t, w, r, "PLRecovered")
			if video == "" {
				return
			}
			inserts++
			remote = append(remote, video)
			if inserts == 1 {
				http.Error(w, "<html>lost item write response</html>", 502)
				return
			}
			fmt.Fprintf(w, `{"id":"new%d"}`, inserts)
		default:
			t.Error("unexpected migration API request")
			http.Error(w, "unexpected", 400)
		}
	})
	mu.Lock()
	queueFile = filepath.Join(h.env["YTPL_STATE_DIR"], "migration-queue.json")
	mu.Unlock()
	h.importOAuth()
	plan := h.write("crash-fixture-plan.json", fmt.Sprintf(migrateFixture, "null"))
	decodeResponse[any](t, h.run("", "migrate", "init", "--plan", plan, "--json"), 0)
	run := func() migrationResult {
		t.Helper()
		return decodeResponse[migrationResult](t, h.run("", "migrate", "run", "--json"), 0).Data
	}
	first := run()
	if first.Created != 0 || first.WriteAttempts != 1 || !first.Playlists[0].CreationPending ||
		first.Playlists[0].Status != "paused" {
		t.Fatalf("lost create response = %+v", first)
	}
	second := run()
	if second.WriteAttempts != 0 || second.Recovered != 0 || !second.Playlists[0].CreationPending ||
		!strings.Contains(second.Playlists[0].Error, "--confirm-recreate") {
		t.Fatalf("empty recovery scan = %+v", second)
	}
	third := run()
	if third.Recovered != 1 || third.WriteAttempts != 1 || third.Inserted != 0 ||
		third.Playlists[0].Status != queue.Paused || third.Playlists[0].PendingInsert == nil ||
		third.Playlists[0].TrackCounts[queue.TrackTransient] != 1 {
		t.Fatalf("bound recovery with ambiguous item = %+v", third)
	}
	fourth := run()
	if fourth.WriteAttempts != 1 || fourth.Inserted != 1 || fourth.Playlists[0].Status != queue.Done ||
		fourth.Playlists[0].PendingInsert != nil {
		t.Fatalf("idempotent item readback = %+v", fourth)
	}
	mu.Lock()
	defer mu.Unlock()
	if creates != 1 || scans != 2 || inserts != 2 || !slices.Equal(remote, []string{cliVideoA, cliVideoB}) {
		t.Fatalf("creates=%d scans=%d inserts=%d remote=%v", creates, scans, inserts, remote)
	}
}

func TestMigrationKeepsLegacyEquivalentsWhenPlanGrowsAndReorders(t *testing.T) {
	const plan = `{"playlists":[{"neteasePlaylistId":"1","name":"新增曲目后的歌单","tracks":[
		{"neteaseSongId":"12","videoId":"bbbbbbbbbbb"},
		{"neteaseSongId":"13","videoId":"eeeeeeeeeee"},
		{"neteaseSongId":"11","videoId":"aaaaaaaaaaa"},
		{"neteaseSongId":"14","videoId":"fffffffffff"}
	]}]}`
	const record = `{"playlists":{"1":{"youtubeId":"PLLegacy","verified":true,
		"requestedVideoIds":["aaaaaaaaaaa","bbbbbbbbbbb"],"videoIds":["ccccccccccc","ddddddddddd"]}}}`
	var mu sync.Mutex
	var inserts []string
	h := harness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/youtube/v3/playlistItems" && r.Method == "GET" && r.URL.Query().Get("playlistId") == "PLLegacy":
			writeItems(t, w, []string{"ccccccccccc", "ddddddddddd"})
		case r.URL.Path == "/youtube/v3/playlistItems" && r.Method == "POST":
			if video := insertVideo(t, w, r, "PLLegacy"); video != "" {
				inserts = append(inserts, video)
				fmt.Fprintf(w, `{"id":"new%d"}`, len(inserts))
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	})
	h.importOAuth()
	initial := decodeResponse[queue.State](t, h.run("", "migrate", "init", "--json",
		"--plan", h.write("plan.json", plan), "--record", h.write("record.json", record)), 0).Data.Playlists[0]
	if initial.Status != queue.Pending ||
		!slices.Equal(initial.VerifiedRequestedVideoIDs, []string{cliVideoA, cliVideoB}) ||
		!slices.Equal(initial.VerifiedActualVideoIDs, []string{"ccccccccccc", "ddddddddddd"}) {
		t.Fatalf("imported verified equivalences = %+v", initial)
	}
	report := decodeResponse[migrationResult](t, h.run("", "migrate", "run", "--json"), 0).Data
	if report.Inserted != 2 || report.WriteAttempts != 2 || report.Playlists[0].Status != queue.Done ||
		report.Playlists[0].ExtraRemoteTracksCount != 0 {
		t.Fatalf("reconciled legacy playlist = %+v", report)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(inserts, []string{"eeeeeeeeeee", "fffffffffff"}) {
		t.Fatalf("inserted = %v, want only the two new tracks", inserts)
	}
}

func TestMigrationPlaylistDeletedBeforeInsertOnlyFailsThatPlaylist(t *testing.T) {
	const plan = `{"playlists":[
		{"neteasePlaylistId":"1","name":"删除的歌单","youtubePlaylistId":"PLDeleted","tracks":[
			{"neteaseSongId":"11","videoId":"aaaaaaaaaaa"},
			{"neteaseSongId":"12","videoId":"bbbbbbbbbbb"},
			{"neteaseSongId":"13","videoId":"ccccccccccc"}]},
		{"neteasePlaylistId":"2","name":"仍然存在的歌单","youtubePlaylistId":"PLNext","tracks":[
			{"neteaseSongId":"21","videoId":"ddddddddddd"}]}
	]}`
	var mu sync.Mutex
	var requests []string
	h := harness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r.Method)
		if r.URL.Path != "/youtube/v3/playlistItems" {
			t.Errorf("unexpected API path %s", r.URL.Path)
		}
		switch len(requests) {
		case 1, 3:
			id := "PLDeleted"
			if len(requests) == 3 {
				id = "PLNext"
			}
			if r.Method == "GET" && r.URL.Query().Get("playlistId") == id {
				writeItems(t, w, nil)
				return
			}
		case 2:
			if r.Method == "POST" && insertVideo(t, w, r, "PLDeleted") == cliVideoA {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"error":{"errors":[{"reason":"playlistNotFound"}]}}`)
				return
			}
		case 4:
			if r.Method == "POST" && insertVideo(t, w, r, "PLNext") == "ddddddddddd" {
				fmt.Fprint(w, `{"id":"newItem"}`)
				return
			}
		}
		t.Errorf("unexpected request #%d %s", len(requests), r.Method)
		http.Error(w, "unexpected", http.StatusBadRequest)
	})
	h.importOAuth()
	decodeResponse[any](t, h.run("", "migrate", "init", "--plan", h.write("plan.json", plan), "--json"), 0)
	report := decodeResponse[migrationResult](t, h.run("", "migrate", "run", "--json"), 0).Data
	if report.Stopped != nil || report.Inserted != 1 || report.WriteAttempts != 2 || len(report.Playlists) != 2 ||
		report.Playlists[1].Status != queue.Done {
		t.Fatalf("other playlist did not complete: %+v", report)
	}
	state := decodeResponse[queue.State](t, h.run("", "migrate", "status", "--json"), 0).Data
	deleted := state.Playlists[0]
	if deleted.Status != queue.Failed || deleted.PendingInsert != nil || !strings.Contains(deleted.Error, "playlistNotFound") {
		t.Fatalf("deleted playlist = %+v", deleted)
	}
	for _, track := range deleted.Tracks {
		if track.Status != queue.TrackPending {
			t.Fatalf("playlist deletion changed track status: %+v", track)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(requests, []string{"GET", "POST", "GET", "POST"}) {
		t.Fatalf("requests = %v, want only one POST per playlist", requests)
	}
}

func TestMigrationAmbiguousInsertWaitsForNonStaleReadback(t *testing.T) {
	const plan = `{"playlists":[{"neteasePlaylistId":"1","name":"重复曲目","youtubePlaylistId":"PLTarget","tracks":[
		{"neteaseSongId":"11","videoId":"aaaaaaaaaaa"},{"neteaseSongId":"12","videoId":"aaaaaaaaaaa"}
	]}]}`
	var mu sync.Mutex
	var queueFile string
	remote := []string{cliVideoA}
	reads, inserts := 0, 0
	h := harness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/youtube/v3/playlistItems" {
			t.Errorf("unexpected API path %s", r.URL.Path)
		}
		switch r.Method {
		case "GET":
			reads++
			if reads == 2 {
				writeItems(t, w, remote[:1])
			} else {
				writeItems(t, w, remote)
			}
		case "POST":
			inserts++
			video := insertVideo(t, w, r, "PLTarget")
			if inserts != 1 || video != cliVideoA {
				t.Errorf("unexpected insert %d for %s", inserts, video)
				http.Error(w, "duplicate write", http.StatusBadRequest)
				return
			}
			data, err := os.ReadFile(queueFile)
			var before queue.State
			if err != nil || json.Unmarshal(data, &before) != nil || len(before.Playlists) != 1 {
				t.Errorf("read queue before POST: %v", err)
			} else if pending := before.Playlists[0].PendingInsert; pending == nil || pending.VideoID != video ||
				pending.TrackIndex != 1 || pending.ExpectedMinCount != 2 {
				t.Errorf("expected durable pending insert for second occurrence, got %+v", pending)
			}
			remote = append(remote, video)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected method %s", r.Method)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	})
	mu.Lock()
	queueFile = filepath.Join(h.env["YTPL_STATE_DIR"], "migration-queue.json")
	mu.Unlock()
	h.importOAuth()
	decodeResponse[any](t, h.run("", "migrate", "init", "--plan", h.write("plan.json", plan), "--json"), 0)

	first := decodeResponse[migrationResult](t, h.run("", "migrate", "run", "--json"), 0).Data
	if first.WriteAttempts != 1 || first.Inserted != 0 || first.Playlists[0].Status != queue.Paused ||
		first.Playlists[0].PendingInsert == nil {
		t.Fatalf("ambiguous insert result = %+v", first)
	}
	retry := decodeResponse[queue.RetryResult](t, h.run("", "migrate", "retry-failed", "--json"), 0).Data
	if retry.Tracks != 0 || retry.Playlists != 0 {
		t.Fatalf("retry-failed changed an unresolved insert: %+v", retry)
	}
	stale := decodeResponse[migrationResult](t, h.run("", "migrate", "run", "--json"), 0).Data
	if stale.WriteAttempts != 0 || stale.Inserted != 0 || stale.Playlists[0].Status != queue.Paused ||
		stale.Playlists[0].PendingInsert == nil || !strings.Contains(stale.Playlists[0].Error, "尚未在远端可见") {
		t.Fatalf("stale readback = %+v", stale)
	}
	current := decodeResponse[migrationResult](t, h.run("", "migrate", "run", "--json"), 0).Data
	if current.WriteAttempts != 0 || current.Inserted != 0 || current.Playlists[0].Status != queue.Done ||
		current.Playlists[0].PendingInsert != nil || current.Playlists[0].TrackCounts[queue.TrackDone] != 2 {
		t.Fatalf("current readback = %+v", current)
	}
	mu.Lock()
	defer mu.Unlock()
	if reads != 3 || inserts != 1 || !slices.Equal(remote, []string{cliVideoA, cliVideoA}) {
		t.Fatalf("remote = %v, %d GETs and %d POSTs", remote, reads, inserts)
	}
}

func TestMigrationHonorsLongRetryAfterAcrossRuns(t *testing.T) {
	const plan = `{"playlists":[{"neteasePlaylistId":"1","name":"受限歌单","youtubePlaylistId":"PLTarget","tracks":[
		{"neteaseSongId":"11","videoId":"aaaaaaaaaaa"},
		{"neteaseSongId":"12","videoId":"bbbbbbbbbbb"},
		{"neteaseSongId":"13","videoId":"ccccccccccc"}
	]}]}`
	var mu sync.Mutex
	var inserts []string
	reads := 0
	h := harness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/youtube/v3/playlistItems" {
			t.Errorf("unexpected API path %s", r.URL.Path)
		}
		switch r.Method {
		case "GET":
			reads++
			writeItems(t, w, nil)
		case "POST":
			if video := insertVideo(t, w, r, "PLTarget"); video != "" {
				inserts = append(inserts, video)
				if len(inserts) == 1 {
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprint(w, `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`)
				} else {
					fmt.Fprintf(w, `{"id":"new%d"}`, len(inserts))
				}
			}
		default:
			t.Errorf("unexpected method %s", r.Method)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	})
	h.importOAuth()
	decodeResponse[any](t, h.run("", "migrate", "init", "--plan", h.write("plan.json", plan), "--json"), 0)
	retryAt := h.now.Add(120 * time.Second)
	run := func() migrationResult {
		t.Helper()
		return decodeResponse[migrationResult](t, h.run("", "migrate", "run", "--json"), 0).Data
	}
	first := run()
	if first.Stopped == nil || first.Stopped.RetryAfter == nil || !first.Stopped.RetryAfter.Equal(retryAt) ||
		first.Stopped.Code != string(youtube.Transient) || first.WriteAttempts != 1 ||
		first.Playlists[0].Status != queue.Paused || first.Playlists[0].PendingInsert != nil ||
		first.Playlists[0].TrackCounts[queue.TrackPending] != 2 {
		t.Fatalf("rate limit did not immediately pause the batch: %+v", first)
	}
	state := decodeResponse[queue.State](t, h.run("", "migrate", "status", "--json"), 0).Data
	if state.RetryAfter == nil || !state.RetryAfter.Equal(retryAt) {
		t.Fatalf("cooldown was not persisted: %+v", state)
	}
	inCooldown := run()
	if inCooldown.Stopped == nil || inCooldown.Stopped.RetryAfter == nil ||
		!inCooldown.Stopped.RetryAfter.Equal(retryAt) || inCooldown.Units != 0 {
		t.Fatalf("cooldown retry = %+v", inCooldown)
	}
	plain := h.run("", "migrate", "run")
	if plain.code != 0 || !strings.Contains(plain.stdout, retryAt.Format(time.RFC3339)) {
		t.Fatalf("cooldown text output = %+v", plain)
	}
	mu.Lock()
	if reads != 1 || !slices.Equal(inserts, []string{cliVideoA}) {
		t.Errorf("during cooldown: %d reads and POSTs for %v", reads, inserts)
	}
	mu.Unlock()
	h.now = retryAt
	next := run()
	if next.Stopped != nil || next.Inserted != 3 || next.Playlists[0].Status != queue.Done {
		t.Fatalf("resume after cooldown = %+v", next)
	}
}

func TestMigrationConfirmationsRequirePlaylistBeforeAnyAPICall(t *testing.T) {
	h := harness(t, nil)
	h.importOAuth()
	plan := h.write("plan.json", fmt.Sprintf(migrateFixture, "null"))
	record := h.write("record.json", `{"playlists":{"1":{"creating":true}}}`)
	decodeResponse[any](t, h.run("", "migrate", "init", "--plan", plan, "--record", record, "--json"), 0)
	for _, flag := range []string{"--confirm-recreate", "--confirm-reinsert"} {
		t.Run(flag, func(t *testing.T) {
			inv := h.run("", "migrate", "run", flag, "--json")
			resp := decodeResponse[migrationResult](t, inv, 1)
			if resp.Error == nil || !strings.Contains(resp.Error.Message, flag) ||
				!strings.Contains(resp.Error.Message, "--playlist") || resp.Data.Units != 0 {
				t.Fatalf("unscoped confirmation = %+v", resp)
			}
		})
	}
}
