// Package round contains the tenant-aware gameplay runtime for a game-provider
// platform (an RGS). One Engine serves MANY operators; each round is fully
// described by Request -> Result.
//
// What is different from a single-tenant custodial engine:
//
//   - Tenancy. Every request names an operator; the engine resolves the tenant
//     (config + seamless wallet) via the operator Registry, or rejects an
//     unknown operator. Idempotency, RNG seed sessions, locks, and ledger
//     accounts are all namespaced by operator.
//   - Seamless wallet. The engine holds no funds. It calls the operator's
//     wallet (Debit / Credit / Rollback) across a boundary. The provider ledger
//     is a per-operator GGR reconciliation mirror, not a custody record.
//   - Per-(operator,player) locking instead of one global mutex, so unrelated
//     players do not serialise.
//   - Per-(operator,player,clientSeed) nonce sessions, so provable-fairness
//     outcomes never correlate across tenants.
//
// Atomicity still wraps "the whole money loop," but the loop now spans two
// systems. In-process state is guarded by the per-account lock (a DB
// transaction in production); correctness ACROSS the boundary is carried by
// TxRef idempotency and the explicit rollback path — you cannot wrap someone
// else's wallet in your transaction.
package round

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"roundrunner/internal/game"
	"roundrunner/internal/ledger"
	"roundrunner/internal/rng"
	"roundrunner/internal/seamless"
	"roundrunner/internal/tenant"
)

// ErrIdempotencyKeyRequired is returned when a request omits its key.
var ErrIdempotencyKeyRequired = errors.New("round: idempotency key required")

// ErrReplayMismatch means an idempotency key was reused for a different bet — a
// client bug we refuse rather than silently return a stale result for.
var ErrReplayMismatch = errors.New("round: idempotency key reused with a different request")

// GameNotEnabledError means the game is unknown or not enabled for the operator.
type GameNotEnabledError struct {
	OperatorID string
	GameName   string
}

func (e *GameNotEnabledError) Error() string {
	return fmt.Sprintf("round: game %s not enabled for operator %s", e.GameName, e.OperatorID)
}

// AssetNotAllowedError means the stake asset is not allowed for the operator.
type AssetNotAllowedError struct {
	OperatorID string
	AssetKey   string
}

func (e *AssetNotAllowedError) Error() string {
	return fmt.Sprintf("round: asset %s not allowed for operator %s", e.AssetKey, e.OperatorID)
}

// LimitError means the stake violated the operator's per-asset limit.
type LimitError struct{ Msg string }

func (e *LimitError) Error() string { return "round: " + e.Msg }

// Settlement is the post-round disposition of the payout leg.
type Settlement string

const (
	SettlementNoPayout Settlement = "no_payout"
	SettlementCredited Settlement = "credited"
	SettlementPending  Settlement = "payout_pending"
)

// Request fully describes one round across the tenant boundary.
type Request struct {
	OperatorID     string
	PlayerID       string
	GameName       string
	ClientSeed     string
	IdempotencyKey string
	Bet            game.Bet
}

// Result carries everything needed to display AND to audit the round, including
// the revealed server seed and commitment for provable fairness.
type Result struct {
	RoundID        string
	OperatorID     string
	IdempotencyKey string
	Outcome        game.Result
	Settlement     Settlement
	Commitment     string
	ServerSeed     string // revealed after the round so anyone can verify
	ClientSeed     string
	Nonce          uint64
	LedgerSeq      uint64
	PayoutTxRef    string // non-empty only when the round was a win
}

// Engine executes rounds for all operators. Construct one and share it across
// all handlers and goroutines.
type Engine struct {
	games         map[string]game.Game
	registry      *tenant.Registry
	ledger        *ledger.Ledger
	creditRetries int

	mu           sync.Mutex             // guards the maps below
	locks        map[string]*sync.Mutex // lockKey (operator|player) -> mutex
	nonces       map[string]uint64      // seedKey -> next nonce
	seq          uint64                 // monotonic round-id sequence
	seen         map[string]Result      // scopedKey -> stored result
	fingerprints map[string]string      // scopedKey -> request fingerprint
}

// Deps wires the engine to its collaborators.
type Deps struct {
	Games         map[string]game.Game
	Registry      *tenant.Registry
	Ledger        *ledger.Ledger
	CreditRetries int // in-band credit retries before marking a payout pending
}

func NewEngine(d Deps) *Engine {
	return &Engine{
		games:         d.Games,
		registry:      d.Registry,
		ledger:        d.Ledger,
		creditRetries: d.CreditRetries,
		locks:         map[string]*sync.Mutex{},
		nonces:        map[string]uint64{},
		seen:          map[string]Result{},
		fingerprints:  map[string]string{},
	}
}

// Execute runs one round end to end. Safe for concurrent use across operators
// and players.
func (e *Engine) Execute(ctx context.Context, req Request) (Result, error) {
	if req.IdempotencyKey == "" {
		return Result{}, ErrIdempotencyKeyRequired
	}

	// 0. Resolve tenant. Unknown operator fails closed BEFORE any work.
	t, err := e.registry.Resolve(req.OperatorID)
	if err != nil {
		return Result{}, err
	}

	// 1. Validate against THIS tenant's enablement and limits, then the game.
	g := e.games[req.GameName]
	if g == nil || !t.Config.AllowsGame(req.GameName) {
		return Result{}, &GameNotEnabledError{OperatorID: req.OperatorID, GameName: req.GameName}
	}
	if !t.Config.AllowsAsset(req.Bet.Stake.Asset()) {
		return Result{}, &AssetNotAllowedError{OperatorID: req.OperatorID, AssetKey: req.Bet.Stake.Asset().Key()}
	}
	if err := g.ValidateBet(req.Bet); err != nil {
		return Result{}, err
	}
	if err := e.enforceTenantLimits(t.Config, req.Bet); err != nil {
		return Result{}, err
	}

	lockKey := req.OperatorID + "|" + req.PlayerID
	scopedKey := req.OperatorID + "|" + req.IdempotencyKey
	seedKey := req.OperatorID + "|" + req.PlayerID + "|" + req.ClientSeed

	// Serialise per (operator, player). Unrelated players never contend; in
	// production this is a DB row lock / transaction.
	am := e.lockFor(lockKey)
	am.Lock()
	defer am.Unlock()

	// Tenant-scoped idempotency: (operatorId, key), never the bare key.
	fp := fingerprint(req)
	if prev, ok := e.getSeen(scopedKey); ok {
		if e.fingerprintOf(scopedKey) != fp {
			return Result{}, ErrReplayMismatch
		}
		return prev, nil
	}

	// 2. Commit to a server seed, derive ids and per-leg TxRefs.
	seed, commitment, err := rng.NewServerSeed()
	if err != nil {
		return Result{}, err
	}
	nonce := e.nextNonce(seedKey)
	roundID := fmt.Sprintf("%s-r-%d-%d", req.OperatorID, e.nextSeq(), nonce)
	stakeTxRef := roundID + ":bet"
	payoutTxRef := roundID + ":win"

	// 3. Debit the stake via the operator's wallet. A decline is a normal
	//    rejection (insufficient funds / limit / excluded) — propagate it; no
	//    outcome is generated.
	if _, err := t.Wallet.Debit(ctx, seamless.DebitArgs{
		OperatorID: req.OperatorID,
		PlayerID:   req.PlayerID,
		Amount:     req.Bet.Stake,
		TxRef:      stakeTxRef,
		RoundID:    roundID,
		GameName:   req.GameName,
	}); err != nil {
		return Result{}, err
	}

	// From here the stake is held on the operator's books. Anything that fails
	// BEFORE we commit a result must roll the debit back.
	outcome, err := g.Settle(req.Bet, rng.Generate(seed, req.ClientSeed, nonce))
	if err != nil {
		e.rollback(ctx, t.Wallet, req, stakeTxRef)
		return Result{}, err
	}

	// 5. Record to the provider reconciliation ledger. Accounts are namespaced
	//    by operator; this is a GGR mirror, NOT custody.
	rec, err := e.ledger.Append(ledger.Journal{
		RoundID:  roundID,
		Memo:     req.OperatorID + ":" + g.Name(),
		Postings: e.postings(req, outcome),
	})
	if err != nil {
		e.rollback(ctx, t.Wallet, req, stakeTxRef)
		return Result{}, fmt.Errorf("round: ledger append failed: %w", err)
	}

	// 6. On a win, credit the payout. A failed credit is RETRIED (idempotent on
	//    payoutTxRef), never rolled back — the player won.
	settlement := SettlementNoPayout
	if outcome.Won {
		settlement = e.creditWithRetry(ctx, t.Wallet, req, outcome, payoutTxRef, roundID)
	}

	res := Result{
		RoundID:        roundID,
		OperatorID:     req.OperatorID,
		IdempotencyKey: req.IdempotencyKey,
		Outcome:        outcome,
		Settlement:     settlement,
		Commitment:     commitment,
		ServerSeed:     fmt.Sprintf("%x", seed),
		ClientSeed:     req.ClientSeed,
		Nonce:          nonce,
		LedgerSeq:      rec.Journal.Seq,
	}
	if outcome.Won {
		res.PayoutTxRef = payoutTxRef
	}
	e.putSeen(scopedKey, res, fp)
	return res, nil
}

// postings are the GGR-mirror legs: the player's operator-side balance is
// mirrored against the operator's GGR pool. Stake moves player -> ggr; a win
// moves ggr -> player. Per-operator GGR = sum over op:<id>:ggr.
func (e *Engine) postings(req Request, outcome game.Result) []ledger.Posting {
	player := fmt.Sprintf("op:%s:player:%s", req.OperatorID, req.PlayerID)
	ggr := fmt.Sprintf("op:%s:ggr", req.OperatorID)
	postings := []ledger.Posting{
		{Account: player, Amount: req.Bet.Stake.Neg()},
		{Account: ggr, Amount: req.Bet.Stake},
	}
	if outcome.Won {
		postings = append(postings,
			ledger.Posting{Account: ggr, Amount: outcome.Payout.Neg()},
			ledger.Posting{Account: player, Amount: outcome.Payout},
		)
	}
	return postings
}

func (e *Engine) creditWithRetry(ctx context.Context, w seamless.OperatorWallet, req Request, outcome game.Result, payoutTxRef, roundID string) Settlement {
	for attempt := 0; attempt <= e.creditRetries; attempt++ {
		_, err := w.Credit(ctx, seamless.CreditArgs{
			OperatorID: req.OperatorID,
			PlayerID:   req.PlayerID,
			Amount:     outcome.Payout,
			TxRef:      payoutTxRef, // idempotent: a retry confirms or applies once
			RoundID:    roundID,
			GameName:   req.GameName,
		})
		if err == nil {
			return SettlementCredited
		}
	}
	// Out of retries. The round still happened and is recorded; the payout is
	// owed. Surface it so an out-of-band sweep can re-credit the same TxRef.
	return SettlementPending
}

func (e *Engine) rollback(ctx context.Context, w seamless.OperatorWallet, req Request, stakeTxRef string) {
	// A failed rollback (boundary down) leaves an orphaned debit for
	// reconciliation to sweep; we don't mask the original error with it.
	_, _ = w.Rollback(ctx, seamless.RollbackArgs{
		OperatorID: req.OperatorID,
		PlayerID:   req.PlayerID,
		Asset:      req.Bet.Stake.Asset(),
		BetTxRef:   stakeTxRef,
		TxRef:      stakeTxRef + ":rollback",
	})
}

func (e *Engine) enforceTenantLimits(c tenant.Config, bet game.Bet) error {
	lim, ok := c.LimitFor(bet.Stake.Asset())
	if !ok {
		return nil // game defaults apply (already checked in ValidateBet)
	}
	v := bet.Stake.Value()
	if lim.Min != nil && v.Cmp(lim.Min) < 0 {
		return &LimitError{"stake below operator minimum"}
	}
	if lim.Max != nil && v.Cmp(lim.Max) > 0 {
		return &LimitError{"stake above operator maximum"}
	}
	return nil
}

// ---- map accessors (each takes e.mu briefly) ----

func (e *Engine) lockFor(key string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	mu := e.locks[key]
	if mu == nil {
		mu = &sync.Mutex{}
		e.locks[key] = mu
	}
	return mu
}

func (e *Engine) nextNonce(seedKey string) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := e.nonces[seedKey]
	e.nonces[seedKey] = n + 1
	return n
}

func (e *Engine) nextSeq() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.seq
	e.seq++
	return s
}

func (e *Engine) getSeen(scopedKey string) (Result, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.seen[scopedKey]
	return r, ok
}

func (e *Engine) fingerprintOf(scopedKey string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.fingerprints[scopedKey]
}

func (e *Engine) putSeen(scopedKey string, res Result, fp string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen[scopedKey] = res
	e.fingerprints[scopedKey] = fp
}

func fingerprint(req Request) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%d|%s",
		req.OperatorID, req.PlayerID, req.GameName,
		req.Bet.Stake.Asset().Key(), req.Bet.Stake.Value().String(),
		req.Bet.Target, req.ClientSeed)
}
