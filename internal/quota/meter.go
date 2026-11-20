package quota

import "fmt"

// Meter belongs to one sequential CLI invocation. The ledger serializes daily
// accounting across processes, while the meter also caps this run's attempts.
type Meter struct {
	Ledger         *Ledger
	EnforceReserve bool
	Budget         *int
	MaxWrites      *int
	Usage          Usage
}

type Usage struct {
	Units         int `json:"units"`
	WriteAttempts int `json:"writeAttempts"`
}

type BudgetError struct {
	Writes bool
	Limit  int
}

func (e *BudgetError) Error() string {
	if e.Writes {
		return fmt.Sprintf("已达到本次运行的写入尝试上限 (%d)", e.Limit)
	}
	return fmt.Sprintf("本次运行的配额预算不足 (%d 单位)", e.Limit)
}

func (m *Meter) Charge(op string, cost int, write bool) error {
	if m.Budget != nil && cost > *m.Budget-m.Usage.Units {
		return &BudgetError{Limit: *m.Budget}
	}
	if write && m.MaxWrites != nil && m.Usage.WriteAttempts >= *m.MaxWrites {
		return &BudgetError{Writes: true, Limit: *m.MaxWrites}
	}
	if err := m.Ledger.Charge(op, cost, ChargeOptions{EnforceReserve: m.EnforceReserve}); err != nil {
		return err
	}
	m.Usage.Units += cost
	if write {
		m.Usage.WriteAttempts++
	}
	return nil
}
