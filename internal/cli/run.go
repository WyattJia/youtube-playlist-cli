// Package cli parses command lines and renders results for ytpl.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/WyattJia/youtube-playlist-cli/internal/auth"
	"github.com/WyattJia/youtube-playlist-cli/internal/playlist"
	"github.com/WyattJia/youtube-playlist-cli/internal/quota"
	"github.com/WyattJia/youtube-playlist-cli/internal/store"
	"github.com/WyattJia/youtube-playlist-cli/internal/youtube"
)

const version = "0.1.0"

type Options struct {
	Context    context.Context
	In         io.Reader
	Out        io.Writer
	Err        io.Writer
	Getenv     func(string) string
	Getwd      func() (string, error)
	Now        func() time.Time
	HTTPClient *http.Client
	APIBase    string
	TokenURL   string
	Wait       func(context.Context, time.Duration) error
}

type runtime struct {
	opts     Options
	auth     *auth.Manager
	ledger   *quota.Ledger
	stateDir string
	key      string
}

type result struct {
	data any
	text string
}

type envelope struct {
	OK      bool     `json:"ok"`
	Help    string   `json:"help,omitempty"`
	Version string   `json:"version,omitempty"`
	Data    any      `json:"data,omitempty"`
	Error   *failure `json:"error,omitempty"`
}

type failure struct {
	Code           string     `json:"code"`
	Message        string     `json:"message"`
	HTTPStatus     int        `json:"httpStatus,omitempty"`
	Reason         string     `json:"reason,omitempty"`
	GoogleStatus   string     `json:"googleStatus,omitempty"`
	Ambiguous      bool       `json:"ambiguous,omitempty"`
	RetryAfterSecs float64    `json:"retryAfterSeconds,omitempty"`
	ResetAt        *time.Time `json:"resetAt,omitempty"`
}

func Run(argv []string, opts Options) int {
	opts = defaults(opts)
	a, err := parse(argv)
	if err != nil {
		msg := envelope{Error: &failure{Code: "USAGE", Message: err.Error()}}
		return emit(opts, jsonOnParseError(argv), msg, "", 2)
	}
	jsonMode := a.enabled("json")
	switch {
	case a.enabled("version"):
		return emit(opts, jsonMode, envelope{OK: true, Version: version}, "ytpl "+version+"\n", 0)
	case a.enabled("help"), a.command == "", a.command == "auth", a.command == "migrate":
		text := help(a.command)
		return emit(opts, jsonMode, envelope{OK: true, Help: text}, text, 0)
	}

	rt, err := newRuntime(a, opts)
	var res result
	if err == nil {
		res, err = rt.execute(a)
	}
	if err != nil {
		return emit(opts, jsonMode, envelope{Data: res.data, Error: describe(err)}, res.text, 1)
	}
	return emit(opts, jsonMode, envelope{OK: true, Data: res.data}, res.text, 0)
}

func defaults(opts Options) Options {
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Err == nil {
		opts.Err = os.Stderr
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.Getwd == nil {
		opts.Getwd = os.Getwd
	}
	return opts
}

func newRuntime(a args, opts Options) (*runtime, error) {
	stateDir := a.or("state-dir", opts.Getenv("YTPL_STATE_DIR"))
	if stateDir == "" {
		stateDir = ".state"
	}
	if !filepath.IsAbs(stateDir) {
		cwd, err := opts.Getwd()
		if err != nil {
			return nil, fmt.Errorf("定位状态目录: %w", err)
		}
		stateDir = filepath.Join(cwd, stateDir)
	}
	if a.has("api-key") && strings.TrimSpace(a.value("api-key")) == "" {
		return nil, errors.New("--api-key 不能为空")
	}
	key := strings.TrimSpace(a.or("api-key", opts.Getenv("YTPL_API_KEY")))
	if strings.ContainsAny(key, "\r\n\x00") {
		return nil, errors.New("API Key 必须是单行值")
	}
	ledger, err := quota.New(stateDir, opts.Now)
	if err != nil {
		return nil, err
	}
	return &runtime{
		opts: opts, auth: auth.New(stateDir, auth.Options{HTTPClient: opts.HTTPClient, Now: opts.Now, TokenURL: opts.TokenURL}),
		stateDir: stateDir, ledger: ledger, key: key,
	}, nil
}

func (r *runtime) api(meter *quota.Meter) *youtube.Client {
	if meter == nil {
		meter = &quota.Meter{Ledger: r.ledger}
	}
	return youtube.New(youtube.Options{
		BaseURL: r.opts.APIBase, HTTPClient: r.opts.HTTPClient, Tokens: r.auth,
		APIKey: func() (string, error) {
			if r.key != "" {
				return r.key, nil
			}
			return r.auth.APIKey()
		},
		Meter: meter, Now: r.opts.Now, Wait: r.opts.Wait,
	})
}

func (r *runtime) execute(a args) (result, error) {
	switch {
	case strings.HasPrefix(a.command, "auth "), a.command == "whoami":
		return r.authCommand(a)
	case strings.HasPrefix(a.command, "migrate "):
		return r.migrateCommand(a)
	case a.command == "quota":
		if len(a.pos) != 0 {
			return result{}, errors.New("quota 不接收位置参数")
		}
		limit, err := a.intOption("limit", 1)
		if err != nil {
			return result{}, err
		}
		s, err := r.ledger.Configure(limit, a.enabled("reset-exhausted"))
		if err != nil {
			return result{}, err
		}
		return result{s, fmt.Sprintf("配额日 %s，用量 %d/%d，预留 %d，已耗尽 %t\n下次重置 %s（剩余 %d 秒）\n",
			s.Day, s.Used, s.Limit, s.Reserve, s.Exhausted, s.ResetAt.Format(time.RFC3339), s.ResetAfterSeconds)}, nil
	default:
		service := playlist.Service{API: r.api(nil), Cache: store.New(r.stateDir)}
		return r.playlistCommand(a, service)
	}
}

func describe(err error) *failure {
	code := string(youtube.KindOf(err))
	if code == "" {
		code = "ERROR"
	}
	f := &failure{Code: code, Message: err.Error()}
	var apiErr *youtube.APIError
	if errors.As(err, &apiErr) {
		f.HTTPStatus, f.Reason, f.GoogleStatus = apiErr.StatusCode, apiErr.Reason, apiErr.GoogleStatus
		f.Ambiguous = apiErr.Ambiguous
		f.RetryAfterSecs = apiErr.RetryAfter.Seconds()
		if !apiErr.ResetAt.IsZero() {
			f.ResetAt = &apiErr.ResetAt
		}
	}
	var localQuota *quota.LimitError
	if errors.As(err, &localQuota) {
		f.ResetAt = &localQuota.ResetAt
	}
	return f
}

func emit(opts Options, jsonMode bool, msg envelope, plain string, code int) int {
	var err error
	if jsonMode {
		encoder := json.NewEncoder(opts.Out)
		encoder.SetEscapeHTML(false)
		err = encoder.Encode(msg)
	} else {
		if plain != "" {
			_, err = io.WriteString(opts.Out, plain)
		}
		if msg.Error != nil {
			_, reportErr := fmt.Fprintln(opts.Err, oneLine(msg.Error.Message))
			err = errors.Join(err, reportErr)
		}
	}
	if err != nil {
		if _, reportErr := fmt.Fprintln(opts.Err, "无法写入命令输出"); reportErr != nil {
			return 1 // Both output streams are unavailable.
		}
		return 1
	}
	return code
}

// Prevent remote titles or unusual paths from injecting terminal controls.
func oneLine(s string) string {
	return strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return ' '
		}
		return c
	}, s)
}
