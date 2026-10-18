package policy

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"cpa-key-policy/internal/policy/audit"
	"cpa-key-policy/internal/policy/persist"
)

// Balance is a prepaid key's money. Management adjustments are the only
// credits; while the key is in balance mode every billed call is deducted in
// the same ledger lock that books its periods. Billing happens after a call,
// so the balance can end below zero by what was in flight when it ran out.
// NetCreditedNano is the signed sum of every adjustment, so the consumption
// since the first credit is NetCreditedNano − Nano.
type Balance struct {
	Nano            Nano             `json:"balance_nano"`
	NetCreditedNano Nano             `json:"net_credited_nano"`
	Requests        []BalanceRequest `json:"requests,omitempty"`
}

// BalanceRequest remembers an adjustment that carried a request id, so a
// retried request is answered with its first result instead of crediting
// twice. Only the latest balanceRequestsKept are kept per key.
type BalanceRequest struct {
	ID          string `json:"id"`
	AmountNano  Nano   `json:"amount_nano"`
	BalanceNano Nano   `json:"balance_nano"`
}

const (
	balanceRequestsKept = 64
	// maxBalanceAdjustmentUSD bounds one adjustment; with int64 nanodollars a
	// balance can hold about 9.2 billion USD, far beyond any sum of these.
	maxBalanceAdjustmentUSD = 1_000_000
	maxBalanceNoteRunes     = 200
	maxBalanceRequestIDLen  = 128
)

var (
	ErrNotBalanceMode             = errors.New("the key is not in balance mode")
	ErrInvalidBalanceAdjustment   = errors.New("invalid balance adjustment")
	ErrBalanceRequestConflict     = errors.New("request_id was already used for a different amount")
	errBalanceRequestsOutOfBounds = fmt.Errorf("a balance keeps at most %d request ids", balanceRequestsKept)
)

func (b *Balance) clone() *Balance {
	if b == nil {
		return nil
	}
	copy := *b
	copy.Requests = append([]BalanceRequest(nil), b.Requests...)
	return &copy
}

func (b *Balance) validate() error {
	if len(b.Requests) > balanceRequestsKept {
		return errBalanceRequestsOutOfBounds
	}
	for _, request := range b.Requests {
		if strings.TrimSpace(request.ID) == "" {
			return errors.New("balance request id is required")
		}
	}
	return nil
}

// BalanceAdjustment is the outcome of one management adjustment. Duplicate
// means the request id was already applied; the amounts are then those of
// the first application.
type BalanceAdjustment struct {
	KeyID          string  `json:"id"`
	AmountUSD      float64 `json:"amount_usd"`
	BeforeUSD      float64 `json:"balance_before_usd"`
	AfterUSD       float64 `json:"balance_usd"`
	NetCreditedUSD float64 `json:"net_credited_usd"`
	Duplicate      bool    `json:"duplicate"`
}

// AdjustBalance adds amountUSD (negative to deduct) to a balance key's
// balance and persists it before answering, then records an adjust_balance
// audit event. A non-empty requestID makes a retry return the first result.
func (s *Store) AdjustBalance(id string, amountUSD float64, note, requestID string) (BalanceAdjustment, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	id, note, requestID = strings.TrimSpace(id), strings.TrimSpace(note), strings.TrimSpace(requestID)
	if id == "" {
		return BalanceAdjustment{}, errors.New("id is required")
	}
	amount := USDToNano(amountUSD)
	switch {
	case math.IsNaN(amountUSD) || math.IsInf(amountUSD, 0) || amount == 0:
		return BalanceAdjustment{}, fmt.Errorf("%w: amount_usd must be a nonzero amount of at least $0.000000001", ErrInvalidBalanceAdjustment)
	case math.Abs(amountUSD) > maxBalanceAdjustmentUSD:
		return BalanceAdjustment{}, fmt.Errorf("%w: amount_usd must be within ±%d", ErrInvalidBalanceAdjustment, maxBalanceAdjustmentUSD)
	case utf8.RuneCountInString(note) > maxBalanceNoteRunes:
		return BalanceAdjustment{}, fmt.Errorf("%w: note must be at most %d characters", ErrInvalidBalanceAdjustment, maxBalanceNoteRunes)
	case len(requestID) > maxBalanceRequestIDLen:
		return BalanceAdjustment{}, fmt.Errorf("%w: request_id must be at most %d bytes", ErrInvalidBalanceAdjustment, maxBalanceRequestIDLen)
	}
	s.mu.RLock()
	key, exists := s.keys[id]
	balanceMode := exists && key.BalanceMode()
	ledger, path, datasetID := s.usage, s.statePath, s.datasetID
	s.mu.RUnlock()
	if !exists {
		return BalanceAdjustment{}, ErrUnknownKey
	}
	if !balanceMode {
		return BalanceAdjustment{}, ErrNotBalanceMode
	}
	// The callback runs under the ledger lock, so the clock is read here.
	writtenAt := s.clock()()
	s.persistMu.Lock()
	result, err := ledger.adjustBalanceAndPersist(id, amount, requestID, func(through string, keys map[string]liveKey) error {
		return writeLive(persist.UsagePath(path), datasetID, ledger.timezone, through, keys, writtenAt)
	})
	s.persistMu.Unlock()
	if err != nil {
		if errors.Is(err, ErrBalanceRequestConflict) || errors.Is(err, ErrUnknownKey) {
			return BalanceAdjustment{}, err
		}
		return BalanceAdjustment{}, fmt.Errorf("persist balance adjustment: %w", err)
	}
	if result.Duplicate {
		return result, nil
	}
	changes := map[string]audit.Change{
		"amount_usd":  {To: result.AmountUSD},
		"balance_usd": {From: result.BeforeUSD, To: result.AfterUSD},
	}
	if note != "" {
		changes["note"] = audit.Change{To: note}
	}
	if requestID != "" {
		changes["request_id"] = audit.Change{To: requestID}
	}
	s.recordAudit(audit.Event{Action: "adjust_balance", KeyID: id, Changes: changes})
	return result, nil
}

// adjustBalanceAndPersist applies one adjustment and writes the live file
// while mu is held, restoring the previous balance when the write fails, so
// a credit is answered only once it is on disk.
//
// TRADEOFF: like resetWindowAndPersist, the live file is written under the
// ledger lock (about 10–25 ms at 1000 keys). Ceiling: a live file above
// 10 MB; revisit when an adjustment takes more than 50 ms.
func (l *usageLedger) adjustBalanceAndPersist(id string, amount Nano, requestID string, persist func(through string, keys map[string]liveKey) error) (BalanceAdjustment, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	usage := l.entries[id]
	if usage == nil || usage.Cycles == nil {
		return BalanceAdjustment{}, ErrUnknownKey
	}
	previous := usage.Balance
	balance := previous.clone()
	if balance == nil {
		balance = &Balance{}
	}
	if requestID != "" {
		for _, request := range balance.Requests {
			if request.ID != requestID {
				continue
			}
			if request.AmountNano != amount {
				return BalanceAdjustment{}, ErrBalanceRequestConflict
			}
			return BalanceAdjustment{
				KeyID: id, AmountUSD: amount.USD(), BeforeUSD: (request.BalanceNano - amount).USD(), AfterUSD: request.BalanceNano.USD(),
				NetCreditedUSD: balance.NetCreditedNano.USD(), Duplicate: true,
			}, nil
		}
	}
	before := balance.Nano
	balance.Nano += amount
	balance.NetCreditedNano += amount
	if requestID != "" {
		balance.Requests = append(balance.Requests, BalanceRequest{ID: requestID, AmountNano: amount, BalanceNano: balance.Nano})
		if extra := len(balance.Requests) - balanceRequestsKept; extra > 0 {
			balance.Requests = append([]BalanceRequest(nil), balance.Requests[extra:]...)
		}
	}
	previousDirty, previousRevision := l.dirty, l.revision
	usage.Balance = balance
	l.markDirtyLocked()
	if err := persist(l.historyThrough, l.liveSnapshotLocked(l.historyThrough)); err != nil {
		usage.Balance = previous
		l.dirty, l.revision = previousDirty, previousRevision
		return BalanceAdjustment{}, err
	}
	l.dirty = false
	return BalanceAdjustment{
		KeyID: id, AmountUSD: amount.USD(), BeforeUSD: before.USD(), AfterUSD: balance.Nano.USD(), NetCreditedUSD: balance.NetCreditedNano.USD(),
	}, nil
}

// balanceDenial refuses a balance key whose balance is used up. There is no
// automatic reset, so the denial carries no retry time.
func (l *usageLedger) balanceDenial(id string) *Denial {
	l.mu.Lock()
	defer l.mu.Unlock()
	if usage := l.entries[id]; usage != nil && usage.Balance != nil && usage.Balance.Nano > 0 {
		return nil
	}
	return &Denial{Reason: "balance_exhausted"}
}
