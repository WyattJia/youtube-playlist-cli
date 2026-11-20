package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
)

type LoginOptions struct {
	AuthUser *int
	Ready    func(localURL string) error
}

func (m *Manager) Login(ctx context.Context, opts LoginOptions) (err error) {
	c, err := m.loadClient()
	if err != nil {
		return err
	}
	var random [64]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("生成 OAuth state 和 PKCE verifier: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(random[:32])
	verifier := base64.RawURLEncoding.EncodeToString(random[32:])
	challenge := sha256.Sum256([]byte(verifier))

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("启动 OAuth 回环服务: %w", err)
	}
	addr := listener.Addr().String()
	base := "http://" + addr
	redirect := base + "/callback"
	query := url.Values{
		"client_id":             {c.ID},
		"response_type":         {"code"},
		"redirect_uri":          {redirect},
		"scope":                 {Scope},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	if opts.AuthUser != nil {
		query.Set("authuser", strconv.Itoa(*opts.AuthUser))
	}

	result := make(chan error, 1)
	serverError := make(chan error, 1)
	var claimed atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Host != addr || r.Method != http.MethodGet {
			http.Error(w, "Invalid loopback request.", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/start":
			// The credential URI is validated and no request parameter controls this redirect.
			w.Header().Set("Location", c.Auth+"?"+query.Encode())
			w.WriteHeader(http.StatusFound)
		case "/callback":
			params, parseErr := url.ParseQuery(r.URL.RawQuery)
			if parseErr != nil || len(params["state"]) != 1 ||
				subtle.ConstantTimeCompare([]byte(params.Get("state")), []byte(state)) != 1 {
				http.Error(w, "Invalid OAuth state.", http.StatusBadRequest)
				return
			}
			code := params.Get("code")
			if params.Get("error") == "" && (len(params["code"]) != 1 || code == "") {
				http.Error(w, "Missing OAuth authorization code.", http.StatusBadRequest)
				return
			}
			if !claimed.CompareAndSwap(false, true) {
				http.Error(w, "OAuth callback already handled.", http.StatusConflict)
				return
			}
			if params.Get("error") != "" {
				http.Error(w, "Google authorization was not granted.", http.StatusBadRequest)
				result <- errors.New("Google OAuth 未授权，请重新运行 ytpl auth login")
				return
			}

			token, exchangeErr := m.exchange(ctx, c, url.Values{
				"grant_type":    {"authorization_code"},
				"code":          {code},
				"redirect_uri":  {redirect},
				"code_verifier": {verifier},
			})
			if exchangeErr == nil {
				exchangeErr = m.withLock(ctx, func() error {
					current, err := m.loadClient()
					if err != nil {
						return err
					}
					if current != c {
						return errors.New("登录期间 OAuth 客户端已变更，请重新登录")
					}
					if err := ctx.Err(); err != nil {
						return err
					}
					return m.saveToken(token)
				})
			}
			if exchangeErr != nil {
				http.Error(w, "Authorization failed. See the terminal.", http.StatusBadGateway)
				result <- exchangeErr
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			// A browser disconnect here does not undo the saved token.
			if _, err := io.WriteString(w, "Authorization complete. You can close this tab.\n"); err != nil {
				result <- errors.New("OAuth 已保存令牌，但无法向浏览器确认完成")
				return
			}
			result <- nil
		default:
			http.NotFound(w, r)
		}
	})
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       5 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func() {
		serverError <- server.Serve(listener)
	}()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if closeErr := server.Shutdown(closeCtx); closeErr != nil {
			err = errors.Join(err, server.Close(), errors.New("关闭 OAuth 回环服务超时"))
		}
	}()
	if opts.Ready != nil {
		if err := opts.Ready(base + "/start"); err != nil {
			return err
		}
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return fmt.Errorf("OAuth 登录已结束: %w", ctx.Err())
	case <-serverError:
		return errors.New("OAuth 回环服务意外停止")
	}
}
