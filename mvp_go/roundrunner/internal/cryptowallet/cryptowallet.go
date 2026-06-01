// Package cryptowallet is a custodial, multi-asset crypto wallet with a
// pending/confirmed deposit lifecycle.
//
// This is the wallet shape from the SINGLE-TENANT custodial design, where the
// platform itself holds player funds. The game-provider engine (package round)
// does not use it — it calls the operator's seamless wallet instead. It is kept
// because it captures two crypto-specific realities the seamless boundary hides:
//
//  1. Multi-asset balances. An account holds BTC AND ETH AND USDT at once,
//     indexed by asset key.
//  2. Pending vs confirmed. A new on-chain deposit is observed at zero
//     confirmations and is NOT bettable until it reaches the asset's
//     confirmation threshold. Before that it sits in a pending pool — visible to
//     the user, unavailable for play. (A chain reorg can later un-confirm a
//     deposit; a real implementation writes a REVERSAL journal, never mutates
//     history.)
package cryptowallet

import (
	"fmt"
	"sync"

	"roundrunner/internal/asset"
)

// InsufficientFundsError is returned when a debit would overdraw an account.
type InsufficientFundsError struct {
	Account   string
	Requested asset.Amount
	Available asset.Amount
}

func (e *InsufficientFundsError) Error() string {
	return fmt.Sprintf("insufficient funds: account %s has %s but needs %s",
		e.Account, e.Available, e.Requested)
}

// PendingDeposit is an on-chain deposit awaiting confirmations.
type PendingDeposit struct {
	TxHash   string
	Account  string
	Amount   asset.Amount
	Required int
	Current  int
}

// InMemory is a custodial multi-asset wallet.
type InMemory struct {
	mu        sync.Mutex
	confirmed map[string]map[string]asset.Amount // account -> assetKey -> Amount
	pending   map[string]*PendingDeposit         // txHash -> deposit
	applied   map[string]struct{}                // idempotency refs already applied
}

func NewInMemory() *InMemory {
	return &InMemory{
		confirmed: map[string]map[string]asset.Amount{},
		pending:   map[string]*PendingDeposit{},
		applied:   map[string]struct{}{},
	}
}

// ---- Operational hooks (called by chain listeners, not the engine) ----

// NotifyDeposit records a newly-seen on-chain deposit. It joins the pending
// pool with zero confirmations and is NOT credited yet. Deduped on txHash.
func (w *InMemory) NotifyDeposit(account string, amount asset.Amount, txHash string, requiredConfirmations int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.pending[txHash]; ok {
		return
	}
	w.pending[txHash] = &PendingDeposit{
		TxHash:   txHash,
		Account:  account,
		Amount:   amount,
		Required: requiredConfirmations,
		Current:  0,
	}
}

// AdvanceConfirmations updates a pending deposit's confirmation count. Once it
// meets the threshold the deposit moves into the spendable balance.
func (w *InMemory) AdvanceConfirmations(txHash string, confirmations int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	dep, ok := w.pending[txHash]
	if !ok {
		return
	}
	dep.Current = confirmations
	if dep.Current >= dep.Required {
		w.creditInternal(dep.Account, dep.Amount)
		delete(w.pending, txHash)
	}
}

// Fund seeds a confirmed balance directly (demo/test helper).
func (w *InMemory) Fund(account string, amount asset.Amount) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.creditInternal(account, amount)
}

// ---- Engine-facing interface (idempotent on ref) ----

func (w *InMemory) Debit(account string, amount asset.Amount, ref string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, done := w.applied[ref]; done {
		return nil // idempotent replay
	}
	current := w.balanceInternal(account, amount.Asset())
	if current.Lt(amount) {
		return &InsufficientFundsError{Account: account, Requested: amount, Available: current}
	}
	w.subInternal(account, amount)
	w.applied[ref] = struct{}{}
	return nil
}

func (w *InMemory) Credit(account string, amount asset.Amount, ref string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, done := w.applied[ref]; done {
		return nil
	}
	w.creditInternal(account, amount)
	w.applied[ref] = struct{}{}
	return nil
}

// Balance reports the confirmed (bettable) balance for one asset.
func (w *InMemory) Balance(account string, a asset.Asset) asset.Amount {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.balanceInternal(account, a)
}

// PendingFor lists an account's not-yet-confirmed deposits.
func (w *InMemory) PendingFor(account string) []PendingDeposit {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []PendingDeposit
	for _, dep := range w.pending {
		if dep.Account == account {
			out = append(out, *dep)
		}
	}
	return out
}

// ---- internals (callers hold w.mu) ----

func (w *InMemory) balanceInternal(account string, a asset.Asset) asset.Amount {
	m := w.confirmed[account]
	if m == nil {
		return asset.Zero(a)
	}
	if amt, ok := m[a.Key()]; ok {
		return amt
	}
	return asset.Zero(a)
}

func (w *InMemory) creditInternal(account string, amount asset.Amount) {
	m := w.confirmed[account]
	if m == nil {
		m = map[string]asset.Amount{}
		w.confirmed[account] = m
	}
	cur, ok := m[amount.Asset().Key()]
	if !ok {
		cur = asset.Zero(amount.Asset())
	}
	m[amount.Asset().Key()] = cur.Add(amount)
}

func (w *InMemory) subInternal(account string, amount asset.Amount) {
	m := w.confirmed[account]
	cur, ok := m[amount.Asset().Key()]
	if !ok {
		cur = asset.Zero(amount.Asset())
	}
	m[amount.Asset().Key()] = cur.Sub(amount)
}
