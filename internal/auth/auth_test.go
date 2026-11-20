package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	syntheticClient = `{"installed":{"client_id":"test-client.apps.googleusercontent.com","client_secret":"secret-client-123","auth_uri":"https://accounts.google.com/o/oauth2/v2/auth","token_uri":"https://oauth2.googleapis.com/token"}}`
	syntheticToken  = `{"access_token":"secret-access-old","refresh_token":"secret-refresh-old","expires_at":1700000060,"refresh_token_expires_in":604800}`
)

func writeFixture(t *testing.T, dir, name, data string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func importFixture(t *testing.T, m *Manager, tokenData string) {
	t.Helper()
	dir := t.TempDir()
	config := writeFixture(t, dir, "client-source.json", syntheticClient)
	var tokenPath string
	if tokenData != "" {
		tokenPath = writeFixture(t, dir, "token-source.json", tokenData)
	}
	if err := m.Import(t.Context(), config, tokenPath, false); err != nil {
		t.Fatal(err)
	}
}

func TestImportPermissionsStatusAndLogout(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, Options{})
	importFixture(t, m, syntheticToken)
	if err := m.SetAPIKey(t.Context(), "  test-key-private \n"); err != nil {
		t.Fatal(err)
	}
	for pathname, mode := range map[string]os.FileMode{
		m.dir: 0o700, filepath.Join(m.dir, "client.json"): 0o600,
		filepath.Join(m.dir, "token.json"): 0o600, filepath.Join(m.dir, "api-key.txt"): 0o600,
	} {
		info, err := os.Stat(pathname)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("%s has mode %o, want %o", pathname, info.Mode().Perm(), mode)
		}
	}
	if key, err := m.APIKey(); err != nil || key != "test-key-private" {
		t.Fatal("stored API Key was not trimmed")
	}

	status, err := m.Status("")
	if err != nil {
		t.Fatal(err)
	}
	if !status.ClientConfigured || !status.TokenConfigured || !status.RefreshTokenPresent ||
		!status.AccessTokenPresent || !status.APIKeyConfigured || !status.RefreshTokenExpiryHint ||
		len(status.Warnings) != 1 || !strings.Contains(status.Warnings[0], "In Production") {
		t.Fatalf("status = %+v", status)
	}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-client-123", "secret-access-old", "secret-refresh-old", "test-key-private"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("auth status printed a secret")
		}
	}
	if err := m.Logout(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	status, err = m.Status("")
	if err != nil || status.TokenConfigured || !status.ClientConfigured || !status.APIKeyConfigured {
		t.Fatalf("after logout = %+v, %v", status, err)
	}
	if err := m.Logout(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	status, err = m.Status("")
	if err != nil || status.TokenConfigured || status.ClientConfigured || status.APIKeyConfigured {
		t.Fatalf("after logout --all = %+v, %v", status, err)
	}
}

func TestImportRejectsBadEndpointsAndValidatesTokenBeforeSaving(t *testing.T) {
	dir := t.TempDir()
	m := New(t.TempDir(), Options{})
	for _, doc := range []string{
		strings.Replace(syntheticClient, "installed", "web", 1),
		strings.Replace(syntheticClient, "accounts.google.com", "accounts.google.com.evil.invalid", 1),
		strings.Replace(syntheticClient, "https://accounts.google.com", "http://accounts.google.com", 1),
		strings.Replace(syntheticClient, "/v2/auth", "/v2/auth?redirect_uri=http://evil.invalid", 1),
		strings.Replace(syntheticClient, "oauth2.googleapis.com", "evil.invalid", 1),
	} {
		from := writeFixture(t, dir, "client-source.json", doc)
		if err := m.Import(t.Context(), from, "", false); err == nil || strings.Contains(err.Error(), "secret-client-123") {
			t.Fatal("invalid client was accepted or echoed")
		}
	}
	from := writeFixture(t, dir, "client-source.json", syntheticClient)
	bad := writeFixture(t, dir, "token-source.json", `{"expires_at":1770000000}`)
	if err := m.Import(t.Context(), from, bad, false); err == nil {
		t.Fatal("import accepted token with no credentials")
	}
	if s, err := m.Status(""); err != nil || s.ClientConfigured {
		t.Fatalf("failed import modified state: %+v, %v", s, err)
	}
	if err := m.Import(t.Context(), from, "", false); err != nil {
		t.Fatal(err)
	}
	if err := m.Import(t.Context(), from, "", false); err == nil {
		t.Fatal("import without --force overwrote credentials")
	}
	if err := m.Import(t.Context(), from, "", true); err != nil {
		t.Fatal(err)
	}
}

func TestImportNewClientInvalidatesOldToken(t *testing.T) {
	m := New(t.TempDir(), Options{Now: func() time.Time { return time.Unix(1699999000, 0) }})
	importFixture(t, m, syntheticToken)
	if access, err := m.AccessToken(t.Context(), ""); err != nil || access != "secret-access-old" {
		t.Fatal("expected an unexpired token for the original client")
	}
	const replacementID = "replacement.apps.googleusercontent.com"
	dir := t.TempDir()
	sameID := strings.Replace(syntheticClient, "secret-client-123", "new-secret-for-same-client", 1)
	if err := m.Import(t.Context(), writeFixture(t, dir, "same-client.json", sameID), "", true); err != nil {
		t.Fatal(err)
	}
	if access, err := m.AccessToken(t.Context(), ""); err != nil || access != "secret-access-old" {
		t.Fatal("reimport of the same client removed its token")
	}
	newClient := strings.Replace(syntheticClient, "test-client.apps.googleusercontent.com", replacementID, 1)
	if err := m.Import(t.Context(), writeFixture(t, dir, "new-client.json", newClient), "", true); err != nil {
		t.Fatal(err)
	}
	if client, err := m.loadClient(); err != nil || client.ID != replacementID {
		t.Fatalf("replacement client was not saved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.dir, "token.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old token file still exists: %v", err)
	}
	if access, err := m.AccessToken(t.Context(), ""); access != "" || !errors.Is(err, ErrNoToken) {
		t.Fatalf("old token is still usable after replacement: %v", err)
	}
}

func TestImportNewClientAndInvalidTokenKeepsOriginalPair(t *testing.T) {
	m := New(t.TempDir(), Options{Now: func() time.Time { return time.Unix(1699999000, 0) }})
	importFixture(t, m, syntheticToken)
	dir := t.TempDir()
	newClient := strings.Replace(syntheticClient, "test-client", "replacement", 1)
	clientPath := writeFixture(t, dir, "new-client.json", newClient)
	tokenPath := writeFixture(t, dir, "invalid-token.json", `{"expires_at":1770000000}`)
	if err := m.Import(t.Context(), clientPath, tokenPath, true); err == nil || !strings.Contains(err.Error(), "OAuth 令牌文件") {
		t.Fatalf("expected token validation error, got %v", err)
	}
	for name, want := range map[string]string{"client.json": syntheticClient, "token.json": syntheticToken} {
		data, err := os.ReadFile(filepath.Join(m.dir, name))
		if err != nil || string(data) != want {
			t.Fatalf("failed import changed %s: %v", name, err)
		}
	}
}

func TestRefreshAtSixtySecondsAndPreserveRefreshToken(t *testing.T) {
	fixedNow := time.Unix(1700000000, 0)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/token" || r.ParseForm() != nil ||
			r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "secret-refresh-old" ||
			r.Form.Get("client_secret") != "secret-client-123" {
			t.Error("unexpected refresh request")
			http.Error(w, "bad request", 400)
			return
		}
		switch calls.Add(1) {
		case 1:
			fmt.Fprint(w, `{"access_token":"new-access","expires_in":3600}`)
		case 2:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant","error_description":"secret-refresh-old"}`)
		default:
			t.Error("unexpected extra refresh")
		}
	}))
	defer server.Close()
	m := New(t.TempDir(), Options{HTTPClient: server.Client(), TokenURL: server.URL + "/token", Now: func() time.Time { return fixedNow }})
	importFixture(t, m, syntheticToken)
	for range 2 {
		if got, err := m.AccessToken(t.Context(), ""); err != nil || got != "new-access" {
			t.Fatalf("access did not refresh successfully: %v", err)
		}
	}
	stored, err := m.loadToken()
	if err != nil || stored.RefreshToken != "secret-refresh-old" ||
		stored.ExpiresAt != fixedNow.Unix()+3600 || stored.RefreshTokenExpiresIn == nil {
		t.Fatal("refresh did not preserve the refresh token or its expiry hint")
	}
	if _, err := m.AccessToken(t.Context(), "new-access"); !errors.Is(err, ErrExpired) ||
		strings.Contains(err.Error(), "secret-refresh-old") {
		t.Fatalf("invalid_grant returned unexpected error: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("refresh calls = %d, want 2", got)
	}
}

func TestLoginLoopbackPKCE(t *testing.T) {
	type exchange struct {
		Verifier string
		Redirect string
	}
	exchanges := make(chan exchange, 2)
	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || r.Form.Get("grant_type") != "authorization_code" ||
			r.Form.Get("code") != "synthetic-code" || r.Form.Get("client_secret") != "secret-client-123" {
			t.Error("malformed authorization code exchange")
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		exchanges <- exchange{r.Form.Get("code_verifier"), r.Form.Get("redirect_uri")}
		fmt.Fprint(w, `{"access_token":"login-access","refresh_token":"login-refresh","expires_in":3600,"refresh_token_expires_in":604800}`)
	}))
	defer tokenEndpoint.Close()
	now := time.Unix(1700000000, 0)
	m := New(t.TempDir(), Options{TokenURL: tokenEndpoint.URL, HTTPClient: tokenEndpoint.Client(), Now: func() time.Time { return now }})
	importFixture(t, m, "")
	browser := http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	user := 2
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := m.Login(ctx, LoginOptions{
		AuthUser: &user,
		Ready: func(start string) error {
			if !strings.HasPrefix(start, "http://127.0.0.1:") || !strings.HasSuffix(start, "/start") {
				return errors.New("unexpected OAuth loopback address")
			}
			resp, err := browser.Get(start + "?redirect_uri=https://evil.invalid/")
			if err != nil {
				return err
			}
			if err := resp.Body.Close(); err != nil {
				return err
			}
			location, err := resp.Location()
			if err != nil {
				return err
			}
			q := location.Query()
			if resp.StatusCode != 302 || resp.Header.Get("Cache-Control") != "no-store" ||
				location.Host != "accounts.google.com" || q.Get("access_type") != "offline" ||
				q.Get("prompt") != "consent" || q.Get("scope") != Scope || q.Get("authuser") != "2" ||
				q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != strings.TrimSuffix(start, "/start")+"/callback" ||
				len(q.Get("state")) < 40 || strings.Contains(location.String(), "secret-client-123") {
				return errors.New("invalid authorization redirect")
			}
			redirect := q.Get("redirect_uri")
			for _, attempt := range []struct {
				params url.Values
				status int
			}{
				{url.Values{"state": {"invalid"}, "code": {"<script>alert(1)</script>"}}, http.StatusBadRequest},
				{url.Values{"state": {q.Get("state")}, "code": {"synthetic-code"}}, http.StatusOK},
			} {
				resp, err := browser.Get(redirect + "?" + attempt.params.Encode())
				if err != nil {
					return err
				}
				data, readErr := io.ReadAll(resp.Body)
				if err := errors.Join(readErr, resp.Body.Close()); err != nil {
					return err
				}
				if resp.StatusCode != attempt.status || resp.Header.Get("Cache-Control") != "no-store" ||
					strings.Contains(string(data), "<script>") || strings.Contains(string(data), "login-access") {
					return errors.New("unsafe OAuth callback response")
				}
			}
			select {
			case e := <-exchanges:
				challenge := sha256.Sum256([]byte(e.Verifier))
				if len(e.Verifier) < 43 || q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(challenge[:]) ||
					e.Redirect != redirect {
					return errors.New("PKCE challenge did not match exchanged verifier")
				}
			default:
				return errors.New("missing local token exchange")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := m.loadToken()
	if err != nil || tok.AccessToken != "login-access" || tok.RefreshToken != "login-refresh" ||
		tok.ExpiresAt != now.Unix()+3600 {
		t.Fatal("login didn't persist the expected token")
	}
	file, err := os.Stat(filepath.Join(m.dir, "token.json"))
	if err != nil || file.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode failed: %v", err)
	}
}
