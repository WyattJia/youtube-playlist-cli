// Package quota tracks local YouTube Data API charges by Pacific calendar day.
package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
	_ "time/tzdata"

	"github.com/WyattJia/youtube-playlist-cli/internal/fsutil"
)

const (
	DefaultLimit   = 10000
	DefaultReserve = 500
)

type Ledger struct {
	path    string
	now     func() time.Time
	pacific *time.Location
}

func New(stateDir string, now func() time.Time) (*Ledger, error) {
	pacific, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return nil, fmt.Errorf("加载太平洋时区: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	return &Ledger{path: filepath.Join(stateDir, "quota-ledger.json"), now: now, pacific: pacific}, nil
}

type dayState struct {
	Day         string         `json:"day"`
	Used        int            `json:"used"`
	Limit       int            `json:"limit"`
	Reserve     int            `json:"reserve"`
	Exhausted   bool           `json:"exhausted"`
	ByOperation map[string]int `json:"byOperation"`
}

type Snapshot struct {
	dayState
	ResetAt           time.Time `json:"resetAt"`
	ResetAfterSeconds int64     `json:"resetAfterSeconds"`
}

type LimitError struct {
	Reason  string
	ResetAt time.Time
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("QUOTA_EXCEEDED: %s。太平洋时间重置于 %s", e.Reason, e.ResetAt.Format(time.RFC3339))
}

type ChargeOptions struct {
	EnforceReserve bool
}

func (l *Ledger) Charge(op string, cost int, opts ChargeOptions) error {
	if cost <= 0 || op == "" {
		return errors.New("无效的配额计费参数")
	}
	_, err := l.withState(true, func(state *Snapshot) error {
		if state.Exhausted {
			return &LimitError{Reason: "Google 项目配额已用尽", ResetAt: state.ResetAt}
		}
		ceiling := state.Limit
		reason := "本地每日配额不足"
		if opts.EnforceReserve {
			ceiling -= state.Reserve
			reason = "已达到迁移批次限额，保留日配额备用"
		}
		if state.Used > ceiling || cost > ceiling-state.Used {
			return &LimitError{Reason: reason, ResetAt: state.ResetAt}
		}
		state.Used += cost
		state.ByOperation[op]++
		return nil
	})
	return err
}

func (l *Ledger) MarkExhausted() (Snapshot, error) {
	return l.withState(true, func(state *Snapshot) error {
		state.Exhausted = true
		return nil
	})
}

func (l *Ledger) Read() (Snapshot, error) {
	return l.withState(false, nil)
}

func (l *Ledger) Configure(limit *int, resetExhausted bool) (Snapshot, error) {
	if limit != nil && *limit <= 0 {
		return Snapshot{}, errors.New("每日配额上限必须大于 0")
	}
	return l.withState(limit != nil || resetExhausted, func(state *Snapshot) error {
		if limit != nil {
			state.Limit = *limit
		}
		if resetExhausted {
			state.Exhausted = false
		}
		return nil
	})
}

func (l *Ledger) withState(write bool, change func(*Snapshot) error) (Snapshot, error) {
	var state Snapshot
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return state, fmt.Errorf("创建配额状态目录: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := fsutil.WithFileLockContext(ctx, l.path+".lock", func() error {
		var err error
		state, err = l.load()
		if err != nil {
			return err
		}
		if change != nil {
			if err := change(&state); err != nil {
				return err
			}
		}
		if write {
			return fsutil.WriteJSON(l.path, state.dayState, 0o600)
		}
		return nil
	})
	return state, err
}

func (l *Ledger) load() (Snapshot, error) {
	now := l.now().In(l.pacific)
	day := now.Format(time.DateOnly)
	state := dayState{Day: day, Limit: DefaultLimit, Reserve: DefaultReserve}
	data, err := os.ReadFile(l.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &state); err != nil {
			return Snapshot{}, fmt.Errorf("读取配额账本 JSON: %w", err)
		}
		if state.Used < 0 || state.Limit <= 0 || state.Reserve < 0 {
			return Snapshot{}, errors.New("配额账本包含无效数值")
		}
	case !errors.Is(err, fs.ErrNotExist):
		return Snapshot{}, fmt.Errorf("读取配额账本: %w", err)
	}
	if state.Day != day {
		state.Day = day
		state.Used = 0
		state.Exhausted = false
		state.ByOperation = nil
	}
	if state.ByOperation == nil {
		state.ByOperation = make(map[string]int)
	}
	reset := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, l.pacific)
	return Snapshot{
		dayState:          state,
		ResetAt:           reset,
		ResetAfterSeconds: int64((reset.Sub(now) + time.Second - 1) / time.Second),
	}, nil
}
