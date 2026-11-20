package queue

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/WyattJia/youtube-playlist-cli/internal/youtube"
)

const (
	videoA = "aaaaaaaaaaa"
	videoB = "bbbbbbbbbbb"
	videoC = "ccccccccccc"
	videoD = "ddddddddddd"
	videoE = "eeeeeeeeeee"
)

type fakeAPI struct {
	listMine func() ([]youtube.Playlist, error)
	create   func(string, string, string) (youtube.Playlist, error)
	items    func(string) ([]youtube.PlaylistItem, error)
	insert   func(string, string, *int) (youtube.PlaylistItem, error)
}

func (a fakeAPI) ListMyPlaylists(context.Context) ([]youtube.Playlist, error) {
	if a.listMine == nil {
		return nil, errors.New("unexpected ListMyPlaylists")
	}
	return a.listMine()
}

func (a fakeAPI) CreatePlaylist(_ context.Context, title, desc, privacy string) (youtube.Playlist, error) {
	if a.create == nil {
		return youtube.Playlist{}, errors.New("unexpected CreatePlaylist")
	}
	return a.create(title, desc, privacy)
}

func (a fakeAPI) ListPlaylistItems(_ context.Context, id string) ([]youtube.PlaylistItem, error) {
	if a.items == nil {
		return nil, errors.New("unexpected ListPlaylistItems")
	}
	return a.items(id)
}

func (a fakeAPI) InsertPlaylistItem(_ context.Context, playlist, video string, pos *int) (youtube.PlaylistItem, error) {
	if a.insert == nil {
		return youtube.PlaylistItem{}, errors.New("unexpected InsertPlaylistItem")
	}
	return a.insert(playlist, video, pos)
}

func fixture(t *testing.T, plan, record string) (Store, InitOptions) {
	t.Helper()
	dir := t.TempDir()
	s := New(filepath.Join(dir, "state"), "")
	planPath := filepath.Join(dir, "YouTube Music迁移计划.json")
	if err := os.WriteFile(planPath, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	var recordPath string
	if record != "" {
		recordPath = filepath.Join(dir, "迁移记录.json")
		if err := os.WriteFile(recordPath, []byte(record), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return s, InitOptions{Plan: planPath, Record: recordPath}
}

func initialized(t *testing.T, plan, record string) (Store, State) {
	t.Helper()
	s, options := fixture(t, plan, record)
	q, err := s.Init(options)
	if err != nil {
		t.Fatal(err)
	}
	return s, q
}

func remotePlaylist(id, title, description string) youtube.Playlist {
	p := youtube.Playlist{ID: id}
	p.Snippet.Title, p.Snippet.Description = title, description
	return p
}

func playlistItems(videos ...string) []youtube.PlaylistItem {
	items := make([]youtube.PlaylistItem, 0, len(videos))
	for index, id := range videos {
		item := youtube.PlaylistItem{ID: fmt.Sprintf("item%d", index)}
		item.ContentDetails.VideoID = id
		items = append(items, item)
	}
	return items
}

func onePlaylist(target string, videos ...string) string {
	tracks := make([]string, 0, len(videos))
	for index, video := range videos {
		var videoJSON = "null"
		if video != "" {
			videoJSON = fmt.Sprintf("%q", video)
		}
		tracks = append(tracks, fmt.Sprintf(`{"neteaseSongId":"%d","videoId":%s,"match":"test"}`, index+1, videoJSON))
	}
	return fmt.Sprintf(`{"sourceCommit":"abc1234567","playlists":[{"neteasePlaylistId":"1","name":"迁移歌单","youtubePlaylistId":%s,"tracks":[%s]}]}`,
		target, strings.Join(tracks, ","))
}

func TestLegacyVerificationRecognizesEquivalentIDsAndExtrasWithoutWrites(t *testing.T) {
	s, state := initialized(t, onePlaylist("null", videoA, videoB, ""),
		`{"commit":"abc1234567","playlists":{"1":{"youtubeId":"PLLegacy","verified":true,
		"requestedVideoIds":["aaaaaaaaaaa","bbbbbbbbbbb"],"videoIds":["ccccccccccc","aaaaaaaaaaa"]}}}`)
	p := state.Playlists[0]
	if p.Status != Done || p.YouTubeID != "PLLegacy" || p.Tracks[2].Status != TrackUnmatched ||
		!slices.Equal(p.VerifiedRequestedVideoIDs, []string{videoA, videoB}) ||
		!slices.Equal(p.VerifiedActualVideoIDs, []string{videoC, videoA}) {
		t.Fatalf("legacy state = %+v", p)
	}
	for _, videos := range [][]string{{videoC, videoA}, {videoC, videoA, videoE}} {
		// A direct match of track 1 to A must be moved to C so track 2 can match A.
		api := fakeAPI{items: func(id string) ([]youtube.PlaylistItem, error) {
			if id != "PLLegacy" {
				t.Fatalf("unexpected target %s", id)
			}
			return playlistItems(videos...), nil
		}}
		got, err := s.Run(t.Context(), api, RunOptions{})
		if err != nil || got.Inserted != 0 || got.Created != 0 || got.Playlists[0].Status != Done ||
			got.Playlists[0].ExtraRemoteTracksCount != len(videos)-2 {
			t.Fatalf("legacy reconciliation = %+v, %v", got, err)
		}
	}
}

func TestLegacyRequestedFallbackAndMultisetOfMissingVideos(t *testing.T) {
	s, initial := initialized(t, onePlaylist(`"PLlegacy"`, videoB, videoA, videoB, videoC),
		`{"commit":"abc1234567","playlists":{"1":{"youtubeId":"PLlegacy","verified":true,"videoIds":["bbbbbbbbbbb","aaaaaaaaaaa","bbbbbbbbbbb","ccccccccccc"]}}}`)
	if initial.Playlists[0].Status != Done {
		t.Fatalf("expected fallback to videoIds: %+v", initial.Playlists[0])
	}
	var inserted []string
	api := fakeAPI{
		items: func(id string) ([]youtube.PlaylistItem, error) {
			return playlistItems(videoB, videoE), nil
		},
		insert: func(id, video string, pos *int) (youtube.PlaylistItem, error) {
			if pos != nil {
				t.Error("migration should append missing tracks while preserving existing order")
			}
			inserted = append(inserted, video)
			if len(inserted) == 2 {
				// The first insert was recorded before the next remote write.
				current, err := s.load()
				if err != nil || current.Playlists[0].Tracks[1].Status != TrackDone {
					t.Fatalf("first insert not persisted before next call: %+v, %v", current, err)
				}
			}
			return youtube.PlaylistItem{ID: "newItem"}, nil
		},
	}
	report, err := s.Run(t.Context(), api, RunOptions{})
	if err != nil || report.Inserted != 3 || report.Playlists[0].ExtraRemoteTracksCount != 1 ||
		!slices.Equal(inserted, []string{videoA, videoB, videoC}) {
		t.Fatalf("reconciled = %+v, %v, inserted %v", report, err, inserted)
	}
}

func TestVerifiedOccurrencesTakePrecedenceOverNewExactMatches(t *testing.T) {
	s, _ := initialized(t, onePlaylist(`"PLLegacy"`, videoC, videoA, videoA, videoE),
		`{"playlists":{"1":{"youtubeId":"PLLegacy","verified":true,
		"requestedVideoIds":["aaaaaaaaaaa","aaaaaaaaaaa"],"videoIds":["ccccccccccc","ddddddddddd"]}}}`)
	var inserted []string
	api := fakeAPI{
		items: func(string) ([]youtube.PlaylistItem, error) { return playlistItems(videoC, videoD), nil },
		insert: func(_, video string, _ *int) (youtube.PlaylistItem, error) {
			inserted = append(inserted, video)
			return youtube.PlaylistItem{ID: "added-item"}, nil
		},
	}
	report, err := s.Run(t.Context(), api, RunOptions{})
	if err != nil || report.Playlists[0].Status != Done || !slices.Equal(inserted, []string{videoC, videoE}) {
		t.Fatalf("reconciled occurrences = %+v, inserts %v, error %v", report, inserted, err)
	}
}

func TestLegacyCreatingWaitsForEventualVisibility(t *testing.T) {
	s, initial := initialized(t, onePlaylist("null", videoA),
		`{"commit":"abc1234567","playlists":{"1":{"creating":true}}}`)
	if !initial.Playlists[0].CreationPending {
		t.Fatal("legacy creating marker was not imported")
	}
	// One creation happened under the legacy tool. Its playlist is visible only
	// on the second scan, and no further POST is allowed during either run.
	creates, scans := 1, 0
	api := fakeAPI{
		listMine: func() ([]youtube.Playlist, error) {
			scans++
			if scans == 1 {
				return nil, nil
			}
			return []youtube.Playlist{remotePlaylist("PLRecovered", "迁移歌单", "原简介 · "+sourceMarker("1", "abc1234567"))}, nil
		},
		items: func(id string) ([]youtube.PlaylistItem, error) {
			if id != "PLRecovered" {
				t.Fatalf("unexpected target %s", id)
			}
			return playlistItems(videoA), nil
		},
		create: func(_, _, _ string) (youtube.Playlist, error) {
			creates++
			return youtube.Playlist{}, errors.New("duplicate creation")
		},
	}
	first, err := s.Run(t.Context(), api, RunOptions{})
	if err != nil || !first.Playlists[0].CreationPending || first.Playlists[0].Status != Paused ||
		!strings.Contains(first.Playlists[0].Error, "--confirm-recreate") {
		t.Fatalf("empty scan = %+v, %v", first, err)
	}
	next, err := s.Run(t.Context(), api, RunOptions{})
	if err != nil || next.Recovered != 1 || next.Inserted != 0 || next.Playlists[0].CreationPending ||
		next.Playlists[0].YouTubeID != "PLRecovered" || creates != 1 {
		t.Fatalf("recovery = %+v, %v, total creates %d", next, err, creates)
	}
}

func TestConfirmRecreateAndAmbiguousCreation(t *testing.T) {
	s, _ := initialized(t, onePlaylist("null"),
		`{"commit":"abc1234567","playlists":{"1":{"creating":true}}}`)
	creates := 0
	api := fakeAPI{
		listMine: func() ([]youtube.Playlist, error) { return nil, nil },
		create: func(title, description, privacy string) (youtube.Playlist, error) {
			creates++
			state, err := s.load()
			if err != nil || !state.Playlists[0].CreationPending || privacy != "private" ||
				title != "迁移歌单" || description != "网易云音乐歌单 ID 1 · 备份 abc1234" {
				t.Fatalf("create without durable pending state or expected metadata: %+v %v", state, err)
			}
			if creates == 1 {
				return youtube.Playlist{}, &youtube.APIError{Kind: youtube.Transient, Ambiguous: true, Message: "lost create response"}
			}
			return youtube.Playlist{ID: "PLConfirmed"}, nil
		},
		items: func(id string) ([]youtube.PlaylistItem, error) { return nil, nil },
	}
	first, err := s.Run(t.Context(), api, RunOptions{Playlist: "1", ConfirmRecreate: true})
	if err != nil || creates != 1 || !first.Playlists[0].CreationPending {
		t.Fatalf("ambiguous create = %+v, %v", first, err)
	}
	paused, err := s.Run(t.Context(), api, RunOptions{})
	if err != nil || creates != 1 || paused.Playlists[0].Status != Paused {
		t.Fatalf("resumed ambiguity = %+v, %v", paused, err)
	}
	created, err := s.Run(t.Context(), api, RunOptions{Playlist: "1", ConfirmRecreate: true})
	if err != nil || creates != 2 || created.Created != 1 || created.Playlists[0].CreationPending ||
		created.Playlists[0].Status != Done || created.Playlists[0].YouTubeID != "PLConfirmed" {
		t.Fatalf("confirmed creation = %+v, %v", created, err)
	}
}

func TestPendingMultipleMatchesConflictEvenWithConfirm(t *testing.T) {
	s, _ := initialized(t, onePlaylist("null"),
		`{"commit":"abc1234567","playlists":{"1":{"creating":true}}}`)
	api := fakeAPI{listMine: func() ([]youtube.Playlist, error) {
		marker := sourceMarker("1", "abc1234567")
		return []youtube.Playlist{
			remotePlaylist("PLZ", "迁移歌单", marker), remotePlaylist("PLA", "迁移歌单", marker),
			remotePlaylist("PLZ", "迁移歌单", marker), remotePlaylist("PLUnrelated", "别的歌单", marker),
		}, nil
	}}
	report, err := s.Run(t.Context(), api, RunOptions{Playlist: "1", ConfirmRecreate: true})
	if err != nil || report.Created != 0 || report.Playlists[0].Status != Conflict {
		t.Fatalf("multiple matches = %+v, %v", report, err)
	}
	q, err := s.Status()
	if err != nil || !slices.Equal(q.Playlists[0].ConflictingYouTubeIDs, []string{"PLA", "PLZ"}) {
		t.Fatalf("conflicting targets = %+v, %v", q, err)
	}
}

func TestInitCommitAndTargetConflictsAndSourceFileProtection(t *testing.T) {
	s, opts := fixture(t, onePlaylist(`"PLPlan"`, videoA),
		`{"commit":"def12345","playlists":{"1":{"youtubeId":"PLRecord"}}}`)
	if _, err := s.Init(opts); err == nil || !strings.Contains(err.Error(), "sourceCommit") {
		t.Fatalf("mismatched commit = %v", err)
	}
	if _, err := os.Stat(s.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed init created queue or stat unexpected: %v", err)
	}
	matchingRecord := `{"commit":"abc1234567","playlists":{"1":{"youtubeId":"PLRecord"}}}`
	if err := os.WriteFile(opts.Record, []byte(matchingRecord), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := s.Init(opts)
	if err != nil {
		t.Fatal(err)
	}
	p := state.Playlists[0]
	if p.Status != Conflict || p.YouTubeID != "" || p.PlanYouTubeID != "PLPlan" || p.RecordYouTubeID != "PLRecord" {
		t.Fatalf("target conflict = %+v", p)
	}
	if _, err := s.Run(t.Context(), fakeAPI{}, RunOptions{Playlist: "1", ConfirmRecreate: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Init(opts); err == nil {
		t.Fatal("existing queue overwritten without --force")
	}
	opts.Force = true
	protected := New(filepath.Dir(s.lock), opts.Record)
	if _, err := protected.Init(opts); err == nil {
		t.Fatal("init --force overwrote source record")
	}
	if bytes, err := os.ReadFile(opts.Record); err != nil || string(bytes) != matchingRecord {
		t.Fatal("source record changed")
	}
}

func TestInitRequiresPlaylistsObjectInExplicitRecord(t *testing.T) {
	for _, record := range []string{`null`, `{}`, `{"playlists":null}`, `{"playlists":[]}`, `{"PLAYLISTS":{}}`, `{"unrelated":{}}`} {
		t.Run(record, func(t *testing.T) {
			s, opts := fixture(t, onePlaylist(`"PLPlan"`, videoA), record)
			if _, err := s.Init(opts); err == nil || !strings.Contains(err.Error(), "playlists JSON 对象") {
				t.Fatalf("invalid record = %v", err)
			}
			if _, err := os.Stat(s.path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid record created a queue: %v", err)
			}
		})
	}
	_, state := initialized(t, onePlaylist(`"PLPlan"`, videoA), `{"playlists":{}}`)
	if len(state.Playlists) != 1 || state.Playlists[0].YouTubeID != "PLPlan" {
		t.Fatalf("empty record affected the plan: %+v", state)
	}
}

func TestScopedErrorsContinueAndRetryPermanent(t *testing.T) {
	const plan = `{"sourceCommit":"abc1234567","playlists":[
		{"neteasePlaylistId":"1","name":"full","youtubePlaylistId":"PLFull","tracks":[{"neteaseSongId":"1","videoId":"aaaaaaaaaaa"}]},
		{"neteasePlaylistId":"2","name":"other","youtubePlaylistId":"PLOther","tracks":[
			{"neteaseSongId":"2","videoId":"bbbbbbbbbbb"},
			{"neteaseSongId":"3","videoId":"ccccccccccc"},
			{"neteaseSongId":"4","videoId":"ddddddddddd"}
		]}]}`
	s, _ := initialized(t, plan, "")
	var attempted []string
	api := fakeAPI{
		items: func(string) ([]youtube.PlaylistItem, error) { return nil, nil },
		insert: func(id, video string, _ *int) (youtube.PlaylistItem, error) {
			attempted = append(attempted, id+"/"+video)
			switch video {
			case videoA:
				return youtube.PlaylistItem{}, &youtube.APIError{Kind: youtube.PlaylistFull, Message: "full"}
			case videoB:
				return youtube.PlaylistItem{}, &youtube.APIError{Kind: youtube.SeriesConflict, Message: "series"}
			case videoC:
				return youtube.PlaylistItem{}, &youtube.APIError{Kind: youtube.ItemNotFound, Reason: "videoNotFound", Message: "missing"}
			default:
				return youtube.PlaylistItem{ID: "added-item"}, nil
			}
		},
	}
	report, err := s.Run(t.Context(), api, RunOptions{})
	if err != nil || report.Inserted != 1 || report.Stopped != nil || !slices.Equal(attempted, []string{
		"PLFull/" + videoA, "PLOther/" + videoB, "PLOther/" + videoC, "PLOther/" + videoD,
	}) {
		t.Fatalf("scoped errors = %+v, %v, attempts %v", report, err, attempted)
	}
	state, err := s.Status()
	if err != nil || state.Playlists[0].Status != Full || state.Playlists[1].Status != Partial ||
		state.Playlists[1].Tracks[0].ErrorCode != string(youtube.SeriesConflict) ||
		state.Playlists[1].Tracks[1].ErrorCode != string(youtube.ItemNotFound) ||
		state.Playlists[1].Tracks[2].Status != TrackDone {
		t.Fatalf("persisted scoped failures = %+v, %v", state, err)
	}
	if got, err := s.RetryFailed(false); err != nil || got.Tracks != 0 || got.Playlists != 0 {
		t.Fatalf("default retry changed permanent failures: %+v, %v", got, err)
	}
	retried, err := s.RetryFailed(true)
	if err != nil || retried.Tracks != 2 || retried.Playlists != 2 {
		t.Fatalf("retry permanent = %+v, %v", retried, err)
	}
	state, err = s.Status()
	if err != nil || state.Playlists[0].Status != Pending || state.Playlists[1].Tracks[0].Status != TrackPending ||
		state.Playlists[1].Tracks[0].Error != "" {
		t.Fatalf("persisted retry = %+v, %v", state, err)
	}
}

func TestTransientThresholdRetryAndReport(t *testing.T) {
	s, _ := initialized(t, onePlaylist(`"PLTest"`, videoA, videoB, videoC, videoD), "")
	attempted := 0
	api := fakeAPI{
		items: func(string) ([]youtube.PlaylistItem, error) { return nil, nil },
		insert: func(_, _ string, _ *int) (youtube.PlaylistItem, error) {
			attempted++
			return youtube.PlaylistItem{}, &youtube.APIError{Kind: youtube.Transient, Message: "temporarily rate limited"}
		},
	}
	report, err := s.Run(t.Context(), api, RunOptions{})
	if err != nil || attempted != 3 || report.Stopped == nil ||
		report.Stopped.Code != string(youtube.Transient) || report.Playlists[0].TrackCounts[TrackTransient] != 3 {
		t.Fatalf("temporary failures = %+v, %v", report, err)
	}

	out := t.TempDir()
	jsonPath, csvPath := filepath.Join(out, "report.json"), filepath.Join(out, "report.csv")
	if err := s.ExportReport(jsonPath, "json"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var exported State
	if err := json.Unmarshal(data, &exported); err != nil || exported.Playlists[0].Tracks[2].Status != TrackTransient ||
		exported.Playlists[0].Tracks[3].Status != TrackPending {
		t.Fatalf("report JSON = %+v, %v", exported, err)
	}
	if err := s.ExportReport(csvPath, "csv"); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil || len(rows) != 5 || rows[0][11] != "track_status" || rows[3][11] != "failed_transient" ||
		rows[4][11] != "pending" {
		t.Fatalf("report CSV = %+v, %v", rows, err)
	}
	if err := s.ExportReport(s.path, "json"); err == nil {
		t.Fatal("report can overwrite queue")
	}
	retried, err := s.RetryFailed(false)
	if err != nil || retried.Tracks != 3 || retried.Playlists != 1 {
		t.Fatalf("retry returned %+v, %v", retried, err)
	}
}

func TestDryRunLeavesQueueBytesUntouchedAndDoesNotCreate(t *testing.T) {
	s, _ := initialized(t, onePlaylist("null", videoA, videoB, ""), "")
	before, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.Run(t.Context(), fakeAPI{}, RunOptions{DryRun: true})
	if err != nil || preview.PlannedCreates != 1 || preview.PlannedInserts != 2 || preview.Created != 0 {
		t.Fatalf("dry run = %+v, %v", preview, err)
	}
	after, err := os.ReadFile(s.path)
	if err != nil || string(after) != string(before) {
		t.Fatal("dry run changed queue")
	}
}

func TestRetryReadbackReservesOccurrenceForConfirmedInsert(t *testing.T) {
	s, _ := initialized(t, onePlaylist(`"PLTarget"`, videoA, videoB),
		`{"playlists":{"1":{"youtubeId":"PLTarget","verified":true,
		"requestedVideoIds":["aaaaaaaaaaa","bbbbbbbbbbb"],"videoIds":["bbbbbbbbbbb","bbbbbbbbbbb"]}}}`)
	var remote []youtube.PlaylistItem
	var inserted []string
	api := fakeAPI{
		items: func(string) ([]youtube.PlaylistItem, error) { return remote, nil },
		insert: func(_, video string, _ *int) (youtube.PlaylistItem, error) {
			inserted = append(inserted, video)
			if video == videoA {
				return youtube.PlaylistItem{}, &youtube.APIError{Kind: youtube.ItemNotFound, Reason: "videoNotFound", Message: "missing"}
			}
			remote = append(remote, playlistItems(videoB)...)
			return youtube.PlaylistItem{}, &youtube.APIError{Kind: youtube.Transient, Ambiguous: true, Message: "lost insert response"}
		},
	}
	first, err := s.Run(t.Context(), api, RunOptions{})
	if err != nil || first.Playlists[0].PendingInsert == nil || first.Playlists[0].PendingInsert.TrackIndex != 1 {
		t.Fatalf("pending insert = %+v, %v", first, err)
	}
	for range 2 {
		next, err := s.Run(t.Context(), api, RunOptions{})
		if err != nil || next.Playlists[0].PendingInsert != nil || next.Inserted != 0 {
			t.Fatalf("readback = %+v, %v", next, err)
		}
	}
	state, err := s.Status()
	if err != nil || state.Playlists[0].Tracks[0].Status != TrackPermanent || state.Playlists[0].Tracks[1].Status != TrackDone ||
		!slices.Equal(inserted, []string{videoA, videoB}) {
		t.Fatalf("remote occurrence assigned to the wrong track: %+v, writes %v, %v", state, inserted, err)
	}
}

func TestConfirmReinsertCountsVerifiedEquivalentOccurrences(t *testing.T) {
	s, _ := initialized(t, onePlaylist(`"PLTarget"`, videoA, videoA),
		`{"playlists":{"1":{"youtubeId":"PLTarget","verified":true,
		"requestedVideoIds":["aaaaaaaaaaa","aaaaaaaaaaa"],"videoIds":["ccccccccccc","ccccccccccc"]}}}`)
	attempts := 0
	api := fakeAPI{
		items: func(string) ([]youtube.PlaylistItem, error) { return playlistItems(videoC), nil },
		insert: func(_, video string, _ *int) (youtube.PlaylistItem, error) {
			if video != videoA {
				t.Fatalf("unexpected insert of %s", video)
			}
			attempts++
			if attempts == 1 {
				return youtube.PlaylistItem{}, &youtube.APIError{Kind: youtube.Transient, Ambiguous: true, Message: "lost insert response"}
			}
			return youtube.PlaylistItem{ID: "added-item"}, nil
		},
	}
	waiting, err := s.Run(t.Context(), api, RunOptions{})
	pending := waiting.Playlists[0].PendingInsert
	if err != nil || pending == nil || pending.TrackIndex != 1 || pending.ExpectedMinCount != 2 || attempts != 1 {
		t.Fatalf("verified equivalent not counted in baseline: %+v, %v", waiting, err)
	}
	recovered, err := s.Run(t.Context(), api, RunOptions{Playlist: "1", ConfirmReinsert: true})
	if err != nil || recovered.Inserted != 1 || recovered.Playlists[0].PendingInsert != nil ||
		recovered.Playlists[0].Status != Done || attempts != 2 {
		t.Fatalf("confirmed reinsert = %+v, %v, attempts %d", recovered, err, attempts)
	}
}

func TestMigrationReportNeutralizesCSVFormulasAndPreservesRawJSON(t *testing.T) {
	const plan = `{"playlists":[{"neteasePlaylistId":"1","name":"  =1+1",
		"tracks":[{"neteaseSongId":"\t11","videoId":"-aaaaaaaaaa","match":"@SUM(1,2)"}]}]}`
	s, _ := initialized(t, plan, "")
	out := t.TempDir()
	csvPath, jsonPath := filepath.Join(out, "report.csv"), filepath.Join(out, "report.json")
	if err := s.ExportReport(csvPath, "csv"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil || len(rows) != 2 || rows[1][2] != "'  =1+1" || rows[1][8] != "'\t11" ||
		rows[1][9] != "'-aaaaaaaaaa" || rows[1][10] != "'@SUM(1,2)" {
		t.Fatalf("report CSV = %+v, %v", rows, err)
	}
	if err := s.ExportReport(jsonPath, "json"); err != nil {
		t.Fatal(err)
	}
	var exported State
	if err := readJSON(jsonPath, "报告", &exported); err != nil {
		t.Fatal(err)
	}
	p := exported.Playlists[0]
	track := p.Tracks[0]
	if p.Name != "  =1+1" || track.SourceID != "\t11" || track.VideoID != "-aaaaaaaaaa" || track.Match != "@SUM(1,2)" {
		t.Fatalf("report JSON altered raw values: %+v", p)
	}
}

func TestDryRunPersistsRetryAfterCooldown(t *testing.T) {
	s, _ := initialized(t, onePlaylist(`"PLTarget"`, videoA), "")
	now := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)
	calls := 0
	api := fakeAPI{
		items: func(string) ([]youtube.PlaylistItem, error) {
			calls++
			return nil, &youtube.APIError{
				Kind:       youtube.Transient,
				RetryAfter: 2 * time.Minute,
				RetryAt:    now.Add(2 * time.Minute),
				Message:    "rate limited",
			}
		},
	}
	preview, err := s.Run(t.Context(), api, RunOptions{DryRun: true, Now: func() time.Time { return now }})
	if err != nil || preview.Stopped == nil || preview.Stopped.RetryAfter == nil || !preview.Stopped.RetryAfter.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("dry run retry-after = %+v, %v", preview, err)
	}
	// Immediate live run must observe persisted cooldown and make 0 API requests.
	live, err := s.Run(t.Context(), api, RunOptions{Now: func() time.Time { return now.Add(30 * time.Second) }})
	if err != nil || live.Stopped == nil || live.Stopped.RetryAfter == nil || calls != 1 {
		t.Fatalf("live run ignored dry-run cooldown: %+v, calls %d, %v", live, calls, err)
	}
	state, err := s.Status()
	if err != nil || state.Playlists[0].Status != Pending {
		t.Fatalf("dry run mutated playlist status beyond cooldown: %+v, %v", state, err)
	}
}

func TestInsertedEquivalentVideoIDPersistsAcrossRuns(t *testing.T) {
	s, _ := initialized(t, onePlaylist(`"PLTarget"`, videoA), "")
	var remote []youtube.PlaylistItem
	inserts := 0
	api := fakeAPI{
		items: func(string) ([]youtube.PlaylistItem, error) { return remote, nil },
		insert: func(_, video string, _ *int) (youtube.PlaylistItem, error) {
			inserts++
			if video != videoA {
				t.Fatalf("unexpected insert of %s", video)
			}
			item := playlistItems(videoB)[0]
			remote = append(remote, item)
			return item, nil
		},
	}
	first, err := s.Run(t.Context(), api, RunOptions{})
	if err != nil || first.Inserted != 1 || first.Playlists[0].Status != Done {
		t.Fatalf("first run = %+v, %v", first, err)
	}
	second, err := s.Run(t.Context(), api, RunOptions{})
	if err != nil || second.Inserted != 0 || inserts != 1 || second.Playlists[0].Status != Done {
		t.Fatalf("second run re-inserted equivalent video: %+v, inserts %d, %v", second, inserts, err)
	}
	state, err := s.Status()
	if err != nil || state.Playlists[0].Tracks[0].ActualVideoID != videoB {
		t.Fatalf("actualVideoId not persisted: %+v, %v", state.Playlists[0].Tracks[0], err)
	}
}

func TestNewlyCreatedPlaylistSkipsReadAndRetriesPropagationLag(t *testing.T) {
	s, _ := initialized(t, onePlaylist("null", videoA), "")
	reads, inserts, sleeps := 0, 0, 0
	api := fakeAPI{
		create: func(title, desc, privacy string) (youtube.Playlist, error) {
			return youtube.Playlist{ID: "PLNewPropagating"}, nil
		},
		items: func(string) ([]youtube.PlaylistItem, error) {
			reads++
			return nil, &youtube.APIError{Kind: youtube.PlaylistNotFound, Reason: "playlistNotFound", Message: "not propagated yet"}
		},
		insert: func(playlistID, video string, _ *int) (youtube.PlaylistItem, error) {
			inserts++
			if inserts == 1 {
				return youtube.PlaylistItem{}, &youtube.APIError{Kind: youtube.PlaylistNotFound, Reason: "playlistNotFound", Message: "propagating"}
			}
			return playlistItems(video)[0], nil
		},
	}
	res, err := s.Run(t.Context(), api, RunOptions{
		Sleep: func(context.Context, time.Duration) error {
			sleeps++
			return nil
		},
	})
	if err != nil || res.Created != 1 || res.Inserted != 1 || res.Playlists[0].Status != Done ||
		reads != 0 || inserts != 2 || sleeps != 1 {
		t.Fatalf("post-create propagation = %+v, reads=%d inserts=%d sleeps=%d, err=%v", res, reads, inserts, sleeps, err)
	}
}
