// Package seamless models the seamless (transfer) wallet boundary between a
// game provider and an operator.
//
// In a game-provider platform the OPERATOR custodies player funds; the provider
// calls the operator's wallet API on every money event and never holds a
// balance itself. The contract is three idempotent operations, each carrying a
// provider-unique TxRef that the operator dedupes on:
//
//	Debit    (bet / wager)  — reserve+remove the stake, or decline
//	Credit   (win / payout) — add winnings, only on a win
//	Rollback (cancel)       — reverse a previously-applied debit
//
// Idempotency on TxRef is what makes "just retry" safe across an inter-company
// network boundary, where retries are not a possibility but a certainty. A
// retried call returns the original result, never a second money movement.
//
// InMemoryOperatorWallet stands in for a real operator's wallet service
// (HTTP/gRPC in production). It models the two behaviours that make the
// boundary hard: TxRef idempotency, and injectable faults so the rollback and
// retry paths are exercised, not merely described.
package seamless

import (
	"context"
	"fmt"
	"sync"

	"roundrunner/internal/asset"
)

// DebitArgs / CreditArgs / RollbackArgs are the boundary call payloads.
type DebitArgs struct {
	OperatorID string
	PlayerID   string
	Amount     asset.Amount
	TxRef      string
	RoundID    string
	GameName   string
}

type CreditArgs struct {
	OperatorID string
	PlayerID   string
	Amount     asset.Amount
	TxRef      string
	RoundID    string
	GameName   string
}

type RollbackArgs struct {
	OperatorID string
	PlayerID   string
	Asset      asset.Asset
	BetTxRef   string // the debit TxRef being reversed
	TxRef      string // this rollback's own idempotency ref
}

// Result is what each boundary call returns: the player's new balance.
type Result struct {
	Balance asset.Amount
}

// OperatorWallet is the seamless interface the engine depends on. A real
// adapter is an HTTP/gRPC client to one operator; swapping it changes nothing
// in the engine.
type OperatorWallet interface {
	Debit(ctx context.Context, args DebitArgs) (Result, error)
	Credit(ctx context.Context, args CreditArgs) (Result, error)
	Rollback(ctx context.Context, args RollbackArgs) (Result, error)
}

// DeclinedError is a NORMAL outcome: the operator refused a debit (insufficient
// funds, limit hit, excluded player). The round is simply rejected — it is not
// a transient fault and is not retried.
type DeclinedError struct {
	PlayerID  string
	Requested asset.Amount
	Available asset.Amount
}

func (e *DeclinedError) Error() string {
	return fmt.Sprintf("wallet declined: %s has %s, needs %s", e.PlayerID, e.Available, e.Requested)
}

// UnavailableError is a transient, retryable failure (timeout, 503, lost
// response). The provider must NOT assume "didn't happen"; the safe move is to
// retry the same TxRef, which is idempotent.
type UnavailableError struct {
	Op  string
	Msg string
}

func (e *UnavailableError) Error() string {
	if e.Msg == "" {
		e.Msg = "operator wallet unavailable"
	}
	return fmt.Sprintf("%s: %s", e.Op, e.Msg)
}

type appliedEntry struct {
	kind    string // "debit" | "credit" | "rollback"
	result  Result
	debited asset.Amount // set for debits, so a rollback restores exactly it
}

// InMemoryOperatorWallet is a reference operator wallet for demos and tests.
type InMemoryOperatorWallet struct {
	mu       sync.Mutex
	balances map[string]map[string]asset.Amount // playerID -> assetKey -> Amount
	applied  map[string]appliedEntry            // txRef -> entry (boundary idempotency)
	faults   func(op string) error              // optional fault injector
}

// Opts configures a wallet. faults, if set, is consulted on each non-deduped
// call and may return an error to simulate a decline or transient failure.
type Opts struct {
	Faults func(op string) error
}

func NewInMemory(opts Opts) *InMemoryOperatorWallet {
	return &InMemoryOperatorWallet{
		balances: map[string]map[string]asset.Amount{},
		applied:  map[string]appliedEntry{},
		faults:   opts.Faults,
	}
}

// Fund is an operator-side top-up (player deposited, KYC'd, confirmed — all the
// operator's concern). Not part of the seamless contract; a demo/test seed.
func (w *InMemoryOperatorWallet) Fund(playerID string, amount asset.Amount) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.creditInternal(playerID, amount)
}

// Balance reports a player's confirmed balance for one asset.
func (w *InMemoryOperatorWallet) Balance(playerID string, a asset.Asset) asset.Amount {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.balanceInternal(playerID, a)
}

func (w *InMemoryOperatorWallet) Debit(ctx context.Context, args DebitArgs) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if e, ok := w.applied[args.TxRef]; ok {
		return e.result, nil // dedupe on TxRef
	}
	if err := w.maybeFault("debit"); err != nil {
		return Result{}, err
	}
	current := w.balanceInternal(args.PlayerID, args.Amount.Asset())
	if current.Lt(args.Amount) {
		// A decline is NOT recorded under TxRef: a later retry, once the player
		// has funds, must be allowed to succeed.
		return Result{}, &DeclinedError{PlayerID: args.PlayerID, Requested: args.Amount, Available: current}
	}
	w.subInternal(args.PlayerID, args.Amount)
	res := Result{Balance: w.balanceInternal(args.PlayerID, args.Amount.Asset())}
	w.applied[args.TxRef] = appliedEntry{kind: "debit", result: res, debited: args.Amount}
	return res, nil
}

func (w *InMemoryOperatorWallet) Credit(ctx context.Context, args CreditArgs) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if e, ok := w.applied[args.TxRef]; ok {
		return e.result, nil
	}
	if err := w.maybeFault("credit"); err != nil {
		return Result{}, err
	}
	w.creditInternal(args.PlayerID, args.Amount)
	res := Result{Balance: w.balanceInternal(args.PlayerID, args.Amount.Asset())}
	w.applied[args.TxRef] = appliedEntry{kind: "credit", result: res}
	return res, nil
}

// Rollback reverses a previously-applied debit. Idempotent on its own TxRef; a
// no-op if the original debit was never applied (e.g. it had been declined).
func (w *InMemoryOperatorWallet) Rollback(ctx context.Context, args RollbackArgs) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if e, ok := w.applied[args.TxRef]; ok {
		return e.result, nil
	}
	if err := w.maybeFault("rollback"); err != nil {
		return Result{}, err
	}
	if orig, ok := w.applied[args.BetTxRef]; ok && orig.kind == "debit" {
		w.creditInternal(args.PlayerID, orig.debited) // restore exactly the stake
	}
	res := Result{Balance: w.balanceInternal(args.PlayerID, args.Asset)}
	w.applied[args.TxRef] = appliedEntry{kind: "rollback", result: res}
	return res, nil
}

// ---- internals (callers hold w.mu) ----

func (w *InMemoryOperatorWallet) maybeFault(op string) error {
	if w.faults == nil {
		return nil
	}
	return w.faults(op)
}

func (w *InMemoryOperatorWallet) balanceInternal(playerID string, a asset.Asset) asset.Amount {
	m := w.balances[playerID]
	if m == nil {
		return asset.Zero(a)
	}
	if amt, ok := m[a.Key()]; ok {
		return amt
	}
	return asset.Zero(a)
}

func (w *InMemoryOperatorWallet) creditInternal(playerID string, amount asset.Amount) {
	m := w.balances[playerID]
	if m == nil {
		m = map[string]asset.Amount{}
		w.balances[playerID] = m
	}
	cur, ok := m[amount.Asset().Key()]
	if !ok {
		cur = asset.Zero(amount.Asset())
	}
	m[amount.Asset().Key()] = cur.Add(amount)
}

func (w *InMemoryOperatorWallet) subInternal(playerID string, amount asset.Amount) {
	m := w.balances[playerID]
	cur, ok := m[amount.Asset().Key()]
	if !ok {
		cur = asset.Zero(amount.Asset())
	}
	m[amount.Asset().Key()] = cur.Sub(amount)
}
