package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/WyattJia/youtube-playlist-cli/internal/auth"
	"github.com/WyattJia/youtube-playlist-cli/internal/quota"
)

const BaseURL = "https://www.googleapis.com/youtube/v3"

type TokenSource interface {
	AccessToken(ctx context.Context, rejected string) (string, error)
}

type Options struct {
	BaseURL    string
	HTTPClient *http.Client
	Tokens     TokenSource
	APIKey     func() (string, error)
	Meter      *quota.Meter
	Now        func() time.Time
	Wait       func(context.Context, time.Duration) error
}

type Client struct {
	base   string
	http   *http.Client
	tokens TokenSource
	apiKey func() (string, error)
	meter  *quota.Meter
	now    func() time.Time
	wait   func(context.Context, time.Duration) error
}

func New(opts Options) *Client {
	httpClient := http.Client{Timeout: 30 * time.Second}
	if opts.HTTPClient != nil {
		httpClient = *opts.HTTPClient
	}
	// Keep credentials on the configured API origin, including on 30x responses.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if opts.BaseURL == "" {
		opts.BaseURL = BaseURL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Wait == nil {
		opts.Wait = wait
	}
	return &Client{
		base: strings.TrimRight(opts.BaseURL, "/"), http: &httpClient, tokens: opts.Tokens,
		apiKey: opts.APIKey, meter: opts.Meter, now: opts.Now, wait: opts.Wait,
	}
}

type operation struct {
	name, method, path string
	cost               int
	oauth              bool
}

func (c *Client) credentials(ctx context.Context, oauth bool) (token, key string, err error) {
	oauthErr := auth.ErrNoToken
	if c.tokens != nil {
		token, oauthErr = c.tokens.AccessToken(ctx, "")
		if oauthErr == nil && token != "" {
			return token, "", nil
		}
		if oauthErr != nil && !errors.Is(oauthErr, auth.ErrNoToken) && !errors.Is(oauthErr, auth.ErrExpired) {
			return "", "", oauthErr
		}
	}
	if !oauth && c.apiKey != nil {
		key, err := c.apiKey()
		if err != nil {
			return "", "", err
		}
		if key != "" {
			return "", key, nil
		}
	}
	if errors.Is(oauthErr, auth.ErrExpired) {
		return "", "", oauthErr
	}
	if oauth {
		return "", "", &APIError{Kind: AuthRequired, Message: "此操作需要 OAuth，API Key 只支持公开读取。请运行 ytpl auth login"}
	}
	return "", "", &APIError{Kind: AuthRequired, Message: "请运行 ytpl auth login 或配置 YouTube Data API Key，以读取公开内容"}
}

func (c *Client) call(ctx context.Context, op operation, query url.Values, payload, output any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	token, key, err := c.credentials(ctx, op.oauth)
	if err != nil {
		return err
	}
	if key != "" {
		query = maps.Clone(query)
		if query == nil {
			query = make(url.Values)
		}
		query.Set("key", key)
	}
	var data []byte
	if payload != nil {
		data, err = json.Marshal(payload)
		if err != nil {
			return errors.New("无法编码 YouTube Data API 请求")
		}
	}
	target := c.base + "/" + op.path
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}

	const retries = 3
	var deleteNotFoundReason string
	switch op.name {
	case "playlistItems.delete":
		deleteNotFoundReason = "playlistItemNotFound"
	case "playlists.delete":
		deleteNotFoundReason = "playlistNotFound"
	}
	var refreshAttempted, ambiguousDelete bool
	for tried := 0; ; {
		apiErr, err := c.attempt(ctx, op, target, token, data, output)
		if err != nil {
			return err
		}
		if apiErr == nil {
			return nil
		}
		if deleteNotFoundReason != "" {
			if ambiguousDelete && apiErr.StatusCode == http.StatusNotFound && apiErr.Reason == deleteNotFoundReason {
				return nil
			}
			ambiguousDelete = ambiguousDelete || apiErr.Ambiguous
		}
		switch apiErr.Kind {
		case QuotaExceeded:
			if c.meter != nil {
				snapshot, err := c.meter.Ledger.MarkExhausted()
				apiErr.ResetAt = snapshot.ResetAt
				if err != nil {
					return errors.Join(apiErr, fmt.Errorf("记录 Google 配额已用尽: %w", err))
				}
			}
			return apiErr
		case AuthExpired:
			if token == "" || refreshAttempted {
				return apiErr
			}
			refreshAttempted = true
			token, err = c.tokens.AccessToken(ctx, token)
			if errors.Is(err, auth.ErrNoToken) || (err == nil && token == "") {
				return auth.ErrExpired
			}
			if err != nil {
				return err
			}
		case Transient:
			// A lost response to POST could mean the object already exists.
			// Let the caller reconcile it before considering another POST.
			if (op.method == http.MethodPost && apiErr.Ambiguous) || tried == retries {
				return apiErr
			}
			delay := max(time.Duration(1<<tried)*250*time.Millisecond, apiErr.RetryAfter)
			if delay > 30*time.Second {
				return apiErr
			}
			if err := c.wait(ctx, delay); err != nil {
				return err
			}
			tried++
		default:
			return apiErr
		}
	}
}

func (c *Client) attempt(ctx context.Context, op operation, target, token string, data []byte, output any) (*APIError, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, op.method, target, bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("无法构造 YouTube Data API 请求")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	write := op.method != http.MethodGet
	if c.meter != nil {
		if err := c.meter.Charge(op.name, op.cost, write); err != nil {
			return nil, err
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// An HTTP transport error can include the full URL and Authorization.
		if ctx.Err() != nil && !write {
			return nil, ctx.Err()
		}
		return &APIError{Kind: Transient, Message: "连接 YouTube Data API 失败", Ambiguous: write}, nil
	}
	const maxResponseSize = 8 << 20
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	bodyErr := errors.Join(readErr, resp.Body.Close())
	if len(body) > maxResponseSize {
		bodyErr = errors.New("响应超过大小上限")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if bodyErr != nil {
			body = nil
		}
		e := decodeError(resp.StatusCode, resp.Header.Get("Retry-After"), body, c.now())
		e.Ambiguous = write && resp.StatusCode >= 500
		return e, nil
	}
	if bodyErr == nil && (output == nil || json.Unmarshal(body, output) == nil) {
		return nil, nil
	}
	return &APIError{
		Kind: Transient, StatusCode: resp.StatusCode, Message: "无法读取完整的 YouTube Data API 响应",
		Ambiguous: write,
	}, nil
}

func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
