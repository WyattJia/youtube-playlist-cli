package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/WyattJia/youtube-playlist-cli/internal/fsutil"
)

// AccessToken refreshes when the access token is close to expiry. rejected is
// the token used in a 401 response. A concurrent refresh may already have
// replaced that token before this process takes the refresh lock.
func (m *Manager) AccessToken(ctx context.Context, rejected string) (string, error) {
	t, err := m.loadToken()
	if err != nil {
		if rejected != "" && errors.Is(err, ErrNoToken) {
			return "", ErrExpired
		}
		return "", err
	}
	if m.usable(t, rejected) {
		return t.AccessToken, nil
	}

	var access string
	err = m.withLock(ctx, func() error {
		t, err := m.loadToken()
		if err != nil {
			return err
		}
		if m.usable(t, rejected) {
			access = t.AccessToken
			return nil
		}
		if t.RefreshToken == "" {
			return ErrExpired
		}
		client, err := m.loadClient()
		if err != nil {
			return err
		}
		fresh, err := m.exchange(ctx, client, url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {t.RefreshToken},
		})
		if err != nil {
			return err
		}
		if fresh.RefreshToken == "" {
			fresh.RefreshToken = t.RefreshToken
		}
		if fresh.RefreshTokenExpiresIn == nil {
			fresh.RefreshTokenExpiresIn = t.RefreshTokenExpiresIn
		}
		if err := m.saveToken(fresh); err != nil {
			return err
		}
		access = fresh.AccessToken
		return nil
	})
	return access, err
}

func (m *Manager) usable(t token, rejected string) bool {
	return t.AccessToken != "" && t.AccessToken != rejected && t.ExpiresAt > m.now().Unix()+60
}

func (m *Manager) saveToken(t token) error {
	if err := fsutil.WriteJSON(filepath.Join(m.dir, "token.json"), t, 0o600); err != nil {
		return fmt.Errorf("保存 OAuth 令牌: %w", err)
	}
	return nil
}

func (m *Manager) exchange(ctx context.Context, c desktopClient, form url.Values) (token, error) {
	form.Set("client_id", c.ID)
	form.Set("client_secret", c.Secret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return token{}, errors.New("无法构造 OAuth 令牌请求")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return token{}, ctx.Err()
		}
		return token{}, errors.New("连接 Google OAuth 令牌端点失败")
	}
	var result struct {
		token
		ExpiresIn int64  `json:"expires_in"`
		ErrorCode string `json:"error"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result)
	closeErr := resp.Body.Close()
	if result.ErrorCode == "invalid_grant" {
		return token{}, ErrExpired
	}
	if resp.StatusCode != http.StatusOK {
		// Do not expose error_description, which can echo request inputs.
		return token{}, fmt.Errorf("Google OAuth 令牌请求失败 (HTTP %d)", resp.StatusCode)
	}
	now := m.now().Unix()
	if decodeErr != nil || closeErr != nil || result.ErrorCode != "" || result.AccessToken == "" ||
		strings.TrimSpace(result.AccessToken) != result.AccessToken ||
		strings.TrimSpace(result.RefreshToken) != result.RefreshToken ||
		strings.ContainsAny(result.AccessToken+result.RefreshToken, "\r\n\x00") ||
		result.ExpiresIn < 0 || (result.ExpiresIn > 0 && now > math.MaxInt64-result.ExpiresIn) {
		return token{}, errors.New("Google OAuth 返回的令牌响应无效")
	}
	result.ExpiresAt = now + result.ExpiresIn
	return result.token, nil
}
