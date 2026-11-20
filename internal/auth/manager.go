// Package auth stores Google Desktop OAuth credentials and manages access tokens.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WyattJia/youtube-playlist-cli/internal/fsutil"
)

const (
	tokenURL = "https://oauth2.googleapis.com/token"
	Scope    = "https://www.googleapis.com/auth/youtube"

	TestingWarning = "令牌含 refresh_token_expires_in。Google Cloud OAuth 同意屏幕可能处于 Testing，刷新令牌通常 7 天过期。到 Google Auth Platform → Audience 将应用发布为 In Production，然后重新运行 ytpl auth login。"
)

var (
	ErrNoToken = errors.New("需要 OAuth 登录，请运行 ytpl auth login")
	ErrExpired = errors.New("AUTH_EXPIRED: OAuth 刷新令牌不可用或已过期，请运行 ytpl auth login")
)

type Options struct {
	HTTPClient *http.Client
	Now        func() time.Time
	TokenURL   string // Override for a local test server.
}

type Manager struct {
	dir      string
	http     *http.Client
	now      func() time.Time
	tokenURL string
}

func New(stateDir string, opts Options) *Manager {
	client := http.Client{Timeout: 30 * time.Second}
	if opts.HTTPClient != nil {
		client = *opts.HTTPClient
	}
	// The token endpoint receives the OAuth client secret and refresh token.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.TokenURL == "" {
		opts.TokenURL = tokenURL
	}
	return &Manager{dir: filepath.Join(stateDir, "auth"), http: &client, now: opts.Now, tokenURL: opts.TokenURL}
}

type desktopClient struct {
	ID     string `json:"client_id"`
	Secret string `json:"client_secret"`
	Auth   string `json:"auth_uri"`
	Token  string `json:"token_uri"`
}

type token struct {
	AccessToken           string `json:"access_token,omitempty"`
	RefreshToken          string `json:"refresh_token,omitempty"`
	ExpiresAt             int64  `json:"expires_at"`
	RefreshTokenExpiresIn *int64 `json:"refresh_token_expires_in,omitempty"`
}

type Status struct {
	ClientConfigured       bool     `json:"clientConfigured"`
	TokenConfigured        bool     `json:"tokenConfigured"`
	AccessTokenPresent     bool     `json:"accessTokenPresent"`
	RefreshTokenPresent    bool     `json:"refreshTokenPresent"`
	AccessTokenExpiresAt   int64    `json:"accessTokenExpiresAt,omitempty"`
	APIKeyConfigured       bool     `json:"apiKeyConfigured"`
	RefreshTokenExpiryHint bool     `json:"refreshTokenExpiryHint"`
	Warnings               []string `json:"warnings,omitempty"`
}

func parseClient(data []byte) (desktopClient, error) {
	var doc struct {
		Installed desktopClient `json:"installed"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return desktopClient{}, errors.New("OAuth 客户端文件必须是 Google Desktop JSON")
	}
	c := doc.Installed
	if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Secret) == "" {
		return desktopClient{}, errors.New("OAuth 客户端缺少 installed.client_id 或 installed.client_secret")
	}
	u, err := url.Parse(c.Auth)
	if err != nil || u.Scheme != "https" || u.Host != "accounts.google.com" ||
		u.User != nil || u.ForceQuery || u.RawQuery != "" || u.Fragment != "" || c.Token != tokenURL {
		return desktopClient{}, errors.New("OAuth 客户端的授权端点或令牌端点不符合 Google Desktop 配置")
	}
	c.Auth = u.String()
	return c, nil
}

func parseToken(data []byte) (token, error) {
	var t token
	if json.Unmarshal(data, &t) != nil {
		return token{}, errors.New("OAuth 令牌文件格式无效")
	}
	if strings.TrimSpace(t.AccessToken) == "" && strings.TrimSpace(t.RefreshToken) == "" {
		return token{}, errors.New("OAuth 令牌文件缺少 refresh_token 或 access_token")
	}
	if strings.TrimSpace(t.AccessToken) != t.AccessToken || strings.TrimSpace(t.RefreshToken) != t.RefreshToken ||
		strings.ContainsAny(t.AccessToken+t.RefreshToken, "\r\n\x00") {
		return token{}, errors.New("OAuth 令牌包含无效字符")
	}
	return t, nil
}

func (m *Manager) loadClient() (desktopClient, error) {
	data, err := os.ReadFile(filepath.Join(m.dir, "client.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return desktopClient{}, errors.New("请先运行 ytpl auth import --client 导入 Google Desktop 客户端")
	}
	if err != nil {
		return desktopClient{}, fmt.Errorf("读取 OAuth 客户端: %w", err)
	}
	return parseClient(data)
}

func (m *Manager) loadToken() (token, error) {
	data, err := os.ReadFile(filepath.Join(m.dir, "token.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return token{}, ErrNoToken
	}
	if err != nil {
		return token{}, fmt.Errorf("读取 OAuth 令牌: %w", err)
	}
	return parseToken(data)
}

func (m *Manager) Import(ctx context.Context, clientPath, tokenPath string, force bool) error {
	clientData, err := os.ReadFile(clientPath)
	if err != nil {
		return fmt.Errorf("读取 OAuth 客户端来源: %w", err)
	}
	client, err := parseClient(clientData)
	if err != nil {
		return err
	}

	type inputFile struct {
		name string
		data []byte
	}
	files := []inputFile{{"client.json", clientData}}
	if tokenPath != "" {
		data, err := os.ReadFile(tokenPath)
		if err != nil {
			return fmt.Errorf("读取 OAuth 令牌来源: %w", err)
		}
		if _, err := parseToken(data); err != nil {
			return err
		}
		files = append(files, inputFile{"token.json", data})
	}

	return m.withLock(ctx, func() error {
		if !force {
			for _, f := range files {
				_, err := os.Lstat(filepath.Join(m.dir, f.name))
				if err == nil {
					return fmt.Errorf("%s 已存在，使用 --force 覆盖", f.name)
				}
				if !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("检查 OAuth 文件: %w", err)
				}
			}
		}

		previousData, err := os.ReadFile(filepath.Join(m.dir, "client.json"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("读取现有 OAuth 客户端: %w", err)
		}
		if previous, err := parseClient(previousData); err != nil || previous.ID != client.ID {
			// Invalidate the old token before replacing the client so a failed
			// token write cannot leave credentials from different clients.
			if err := os.Remove(filepath.Join(m.dir, "token.json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("移除旧 OAuth 令牌: %w", err)
			}
		}
		for _, f := range files {
			if err := fsutil.AtomicWrite(filepath.Join(m.dir, f.name), f.data, 0o600); err != nil {
				return fmt.Errorf("保存 %s: %w", f.name, err)
			}
		}
		return nil
	})
}

func (m *Manager) APIKey() (string, error) {
	data, err := os.ReadFile(filepath.Join(m.dir, "api-key.txt"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取 API Key: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func (m *Manager) SetAPIKey(ctx context.Context, key string) error {
	key = strings.TrimSpace(key)
	if key == "" || strings.ContainsAny(key, "\r\n\x00") {
		return errors.New("需要非空的单行 API Key")
	}
	return m.withLock(ctx, func() error {
		return fsutil.AtomicWrite(filepath.Join(m.dir, "api-key.txt"), []byte(key+"\n"), 0o600)
	})
}

func (m *Manager) Status(effectiveKey string) (Status, error) {
	s := Status{APIKeyConfigured: effectiveKey != ""}
	if effectiveKey == "" {
		key, err := m.APIKey()
		if err != nil {
			return s, err
		}
		s.APIKeyConfigured = key != ""
	}
	clientPath := filepath.Join(m.dir, "client.json")
	if _, err := os.Stat(clientPath); err == nil {
		if _, err := m.loadClient(); err != nil {
			return s, err
		}
		s.ClientConfigured = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return s, fmt.Errorf("检查 OAuth 客户端: %w", err)
	}
	t, err := m.loadToken()
	if errors.Is(err, ErrNoToken) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	s.TokenConfigured = true
	s.AccessTokenPresent = t.AccessToken != ""
	s.RefreshTokenPresent = t.RefreshToken != ""
	s.AccessTokenExpiresAt = t.ExpiresAt
	s.RefreshTokenExpiryHint = t.RefreshTokenExpiresIn != nil
	if s.RefreshTokenExpiryHint {
		s.Warnings = []string{TestingWarning}
	}
	return s, nil
}

func (m *Manager) Logout(ctx context.Context, all bool) error {
	names := []string{"token.json"}
	if all {
		names = append(names, "client.json", "api-key.txt")
	}
	return m.withLock(ctx, func() error {
		var removalErr error
		for _, name := range names {
			if err := os.Remove(filepath.Join(m.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				removalErr = errors.Join(removalErr, fmt.Errorf("移除 %s: %w", name, err))
			}
		}
		return removalErr
	})
}

func (m *Manager) withLock(ctx context.Context, fn func() error) error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return fmt.Errorf("创建 OAuth 目录: %w", err)
	}
	if err := os.Chmod(m.dir, 0o700); err != nil {
		return fmt.Errorf("设置 OAuth 目录权限: %w", err)
	}
	lockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return fsutil.WithFileLockContext(lockCtx, filepath.Join(m.dir, "auth.lock"), fn)
}
