package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	cliClient = `{"installed":{"client_id":"test-client.apps.googleusercontent.com","client_secret":"cli-secret-client","auth_uri":"https://accounts.google.com/o/oauth2/v2/auth","token_uri":"https://oauth2.googleapis.com/token"}}`
	cliVideoA = "aaaaaaaaaaa"
	cliVideoB = "bbbbbbbbbbb"
)

type cliHarness struct {
	t    *testing.T
	dir  string
	env  map[string]string
	opts Options
	now  time.Time
}

func harness(t *testing.T, handler http.HandlerFunc) *cliHarness {
	t.Helper()
	dir := t.TempDir()
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) {
			t.Error("unexpected outgoing request")
			http.Error(w, "unexpected", 500)
		}
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	h := &cliHarness{
		t: t, dir: dir, env: map[string]string{"YTPL_STATE_DIR": filepath.Join(dir, "state")},
		now: time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC),
	}
	h.opts = Options{
		Context: t.Context(), Getenv: func(key string) string { return h.env[key] },
		Getwd: func() (string, error) { return dir, nil }, Now: func() time.Time { return h.now },
		HTTPClient: server.Client(), APIBase: server.URL + "/youtube/v3",
		TokenURL: server.URL + "/oauth-token", Wait: func(context.Context, time.Duration) error { return nil },
	}
	return h
}

type invocation struct {
	code   int
	stdout string
	stderr string
}

func (h *cliHarness) run(input string, command ...string) invocation {
	var out, stderr bytes.Buffer
	opts := h.opts
	opts.In, opts.Out, opts.Err = strings.NewReader(input), &out, &stderr
	code := Run(command, opts)
	return invocation{code, out.String(), stderr.String()}
}

type cliResponse[T any] struct {
	OK      bool     `json:"ok"`
	Data    T        `json:"data"`
	Error   *failure `json:"error"`
	Help    string   `json:"help"`
	Version string   `json:"version"`
}

func decodeResponse[T any](t *testing.T, inv invocation, code int) cliResponse[T] {
	t.Helper()
	var out cliResponse[T]
	if err := json.Unmarshal([]byte(inv.stdout), &out); err != nil {
		t.Fatalf("stdout was not a single JSON object: %v", err)
	}
	if inv.code != code || out.OK != (code == 0) || inv.stderr != "" {
		t.Fatalf("exit = %d/%d, OK=%t error=%+v stderr=%q", inv.code, code, out.OK, out.Error, inv.stderr)
	}
	return out
}

func (h *cliHarness) write(name, content string) string {
	h.t.Helper()
	path := filepath.Join(h.dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return path
}

func (h *cliHarness) importOAuth() {
	h.t.Helper()
	client := h.write("fake-desktop.json", cliClient)
	tok := h.write("fake-token.json", fmt.Sprintf(`{"access_token":"cli-secret-access","refresh_token":"cli-secret-refresh","expires_at":%d,"refresh_token_expires_in":604800}`, h.now.Unix()+3600))
	decodeResponse[any](h.t, h.run("", "auth", "--json", "import", "--client", client, "--token", tok), 0)
}

func assertNoSecrets(t *testing.T, inv invocation, secrets ...string) {
	t.Helper()
	for _, value := range secrets {
		if strings.Contains(inv.stdout, value) || strings.Contains(inv.stderr, value) {
			t.Fatal("CLI output contained a credential")
		}
	}
}

func TestJSONHelpVersionAndOfflineDryRuns(t *testing.T) {
	h := harness(t, nil)
	for _, command := range [][]string{
		{"--json", "--help"}, {"--help", "--json"}, {"migrate", "--json", "run", "-h"},
	} {
		out := decodeResponse[any](t, h.run("", command...), 0)
		if !strings.Contains(out.Help, "ytpl") {
			t.Fatal("JSON help missing command documentation")
		}
	}
	if out := decodeResponse[any](t, h.run("", "-v", "--json"), 0); out.Version != version {
		t.Fatalf("version = %q", out.Version)
	}
	out := decodeResponse[map[string]any](t, h.run("", "create", "--title=--json", "--dry-run", "--json"), 0)
	if out.Data["dryRun"] != true || out.Data["applied"] != false {
		t.Fatalf("create dry run = %+v", out)
	}
	decodeResponse[any](t, h.run("", "delete", "PLTest", "--dry-run", "--json"), 0)
	rejected := decodeResponse[any](t, h.run("", "delete", "PLTest", "--json"), 1)
	if rejected.Error == nil || !strings.Contains(rejected.Error.Message, "--yes") {
		t.Fatalf("delete without --yes = %+v", rejected)
	}
	syntax := decodeResponse[any](t, h.run("", "create", "--api-key=secret-flag", "--unknown", "--json"), 2)
	if syntax.Error == nil || strings.Contains(syntax.Error.Message, "secret-flag") {
		t.Fatal("unexpected command syntax output")
	}
}

func TestPublicSingleSyncWithKeyAloneAndNoMine(t *testing.T) {
	const key = "private-only-key+1"
	var reads atomic.Int32
	var fail atomic.Bool
	h := harness(t, func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		q := r.URL.Query()
		if r.Method != "GET" || q.Get("key") != key || r.Header.Get("Authorization") != "" || q.Has("mine") {
			t.Error("public sync used the wrong request mode")
			http.Error(w, "invalid request", 400)
			return
		}
		if fail.Load() {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, `{"error":{"status":"PERMISSION_DENIED","errors":[{"reason":"forbidden"}],"message":%q}}`, "bad URL "+r.URL.String())
			return
		}
		switch r.URL.Path {
		case "/youtube/v3/playlists":
			if q.Get("id") != "PLPublic" {
				t.Error("single sync did not select playlist ID")
			}
			fmt.Fprint(w, `{"items":[{"id":"PLPublic","snippet":{"title":"公开歌单"},"status":{"privacyStatus":"public"},"contentDetails":{"itemCount":1}}]}`)
		case "/youtube/v3/playlistItems":
			if q.Get("playlistId") != "PLPublic" {
				t.Error("single sync requested items of wrong playlist")
			}
			fmt.Fprintf(w, `{"items":[{"id":"entry","snippet":{"title":"一首歌","position":0},"contentDetails":{"videoId":%q}}]}`, cliVideoA)
		default:
			t.Error("unexpected public sync endpoint")
		}
	})
	h.env["YTPL_API_KEY"] = "ignored-environment-key"
	alternate := filepath.Join(h.dir, "alternate")
	r := h.run("", "--json", "sync", "--playlist", "https://music.youtube.com/playlist?list=PLPublic", "--state-dir", alternate, "--api-key", key)
	data := decodeResponse[[]map[string]any](t, r, 0)
	assertNoSecrets(t, r, key, "ignored-environment-key")
	if reads.Load() != 2 || len(data.Data) != 1 || data.Data[0]["id"] != "PLPublic" {
		t.Fatalf("single sync = %+v, reads %d", data, reads.Load())
	}
	cached := decodeResponse[map[string]any](t, h.run("", "show", "--state-dir", alternate, "PLPublic", "--json"), 0)
	if reads.Load() != 2 || cached.Data["hasItems"] != true {
		t.Fatal("show did not use the selected local cache")
	}
	remote := decodeResponse[any](t, h.run("", "list", "--remote", "--api-key", key, "--json"), 1)
	if reads.Load() != 2 || remote.Error.Code != "OAUTH_REQUIRED" {
		t.Fatal("mine unexpectedly made a request with only a key")
	}

	fail.Store(true)
	for _, args := range [][]string{
		{"show", "PLPublic", "--remote", "--api-key", key},
		{"show", "PLPublic", "--remote", "--api-key", key, "--json"},
	} {
		bad := h.run("", args...)
		assertNoSecrets(t, bad, key, "private-only-key%2B1", "key=", "Authorization:")
		if bad.code == 0 {
			t.Fatal("server permission error was not surfaced")
		}
	}
}

func TestWhoamiRefreshAndBrandAccountGuidanceWithoutSecretOutput(t *testing.T) {
	var channels, refreshes atomic.Int32
	var missing atomic.Int32
	h := harness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth-token":
			refreshes.Add(1)
			if r.ParseForm() != nil || r.Form.Get("refresh_token") != "cli-secret-refresh" ||
				r.Form.Get("client_secret") != "cli-secret-client" {
				t.Error("invalid mock OAuth refresh")
			}
			fmt.Fprint(w, `{"access_token":"cli-new-access","expires_in":7200}`)
		case "/youtube/v3/channels":
			n := channels.Add(1)
			if r.URL.Query().Get("mine") != "true" || r.URL.Query().Get("part") != "snippet,contentDetails,statistics" {
				t.Error("incorrect oauth identity request")
			}
			if n == 1 {
				if r.Header.Get("Authorization") != "Bearer cli-secret-access" {
					t.Error("did not use initial access token")
				}
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":{"message":"Authorization: Bearer cli-secret-access"}}`)
				return
			}
			if r.Header.Get("Authorization") != "Bearer cli-new-access" || r.URL.Query().Has("key") {
				t.Error("refreshed token or request auth incorrect")
			}
			switch missing.Load() {
			case 1:
				fmt.Fprint(w, `{"items":[]}`)
			case 2:
				w.WriteHeader(404)
				fmt.Fprint(w, `{"error":{"errors":[{"reason":"channelNotFound"}],"message":"cli-secret-client"}}`)
			default:
				fmt.Fprint(w, `{"items":[{"id":"UCBrand","snippet":{"title":"品牌频道","customUrl":"@brand"}}]}`)
			}
		default:
			t.Error("unexpected endpoint")
			http.Error(w, "unexpected", 400)
		}
	})
	h.importOAuth()
	key := h.run("  cli-secret-key \n", "auth", "set-api-key", "--stdin", "--json")
	decodeResponse[any](t, key, 0)
	assertNoSecrets(t, key, "cli-secret-key")
	info := h.run("", "whoami", "--json")
	response := decodeResponse[authResult](t, info, 0)
	assertNoSecrets(t, info, "cli-secret-key", "cli-secret-client", "cli-secret-access", "cli-secret-refresh", "cli-new-access")
	if response.Data.Channel == nil || response.Data.Channel.ID != "UCBrand" || response.Data.Channel.Handle != "@brand" ||
		!response.Data.APIKeyConfigured || !response.Data.RefreshTokenExpiryHint ||
		response.Data.AccessTokenExpiresAt != h.now.Unix()+7200 ||
		len(response.Data.Warnings) != 1 || !strings.Contains(response.Data.Warnings[0], "In Production") ||
		channels.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("unexpected whoami response: %+v", response)
	}
	for _, mode := range []int32{1, 2} {
		missing.Store(mode)
		status := decodeResponse[authResult](t, h.run("", "auth", "status", "--json"), 1)
		if status.Error.Code != "CHANNEL_NOT_FOUND" || !strings.Contains(status.Error.Message, "Brand Account") {
			t.Fatalf("missing channel guidance: %+v", status.Error)
		}
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh happened %d times", refreshes.Load())
	}
	decodeResponse[any](t, h.run("", "auth", "logout", "--all", "--json"), 0)
	after := decodeResponse[authResult](t, h.run("", "auth", "status", "--json"), 0)
	if after.Data.ClientConfigured || after.Data.TokenConfigured || after.Data.APIKeyConfigured {
		t.Fatalf("logout --all left auth configured: %+v", after.Data)
	}
}
