package round_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"roundrunner/internal/asset"
	"roundrunner/internal/game"
	"roundrunner/internal/ledger"
	"roundrunner/internal/rng"
	"roundrunner/internal/round"
	"roundrunner/internal/seamless"
	"roundrunner/internal/tenant"
)

var ctx = context.Background()

const usdtKey = "USDT:tron"

func usdt(v int64) asset.Amount { return asset.NewInt(asset.USDT_TRON, v) }

type setup struct {
	wallet *seamless.InMemoryOperatorWallet
	ledger *ledger.Ledger
	engine *round.Engine
}

func newSetup(t *testing.T, faults func(string) error, creditRetries int) setup {
	t.Helper()
	g, err := game.NewConfig(game.Spec{
		Name: "dice", Asset: asset.USDT_TRON, HouseEdgeBps: 200,
		MinBet: usdt(1), MaxBet: usdt(1_000_000_000),
	})
	if err != nil {
		t.Fatal(err)
	}
	w := seamless.NewInMemory(seamless.Opts{Faults: faults})
	w.Fund("p", usdt(100_000_000))
	reg := tenant.NewRegistry()
	tc, err := tenant.NewConfig(tenant.Spec{
		OperatorID: "op1", EnabledGames: []string{"dice"},
		AllowedAssets: []string{usdtKey}, CallbackSecret: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(tc, w); err != nil {
		t.Fatal(err)
	}
	l := ledger.New()
	e := round.NewEngine(round.Deps{
		Games: map[string]game.Game{"dice": g}, Registry: reg, Ledger: l, CreditRetries: creditRetries,
	})
	return setup{wallet: w, ledger: l, engine: e}
}

func req(over func(*round.Request)) round.Request {
	r := round.Request{
		OperatorID: "op1", PlayerID: "p", GameName: "dice",
		ClientSeed: "s", IdempotencyKey: "k1",
		Bet: game.Bet{Stake: usdt(1_000_000), Target: 5000},
	}
	if over != nil {
		over(&r)
	}
	return r
}

func TestIdempotentReplayDoesNotDoubleCharge(t *testing.T) {
	s := newSetup(t, nil, 1)
	r1, err := s.engine.Execute(ctx, req(nil))
	if err != nil {
		t.Fatal(err)
	}
	after := s.wallet.Balance("p", asset.USDT_TRON).Value()
	r2, err := s.engine.Execute(ctx, req(nil))
	if err != nil {
		t.Fatal(err)
	}
	if s.wallet.Balance("p", asset.USDT_TRON).Value().Cmp(after) != 0 {
		t.Fatal("replay changed the balance")
	}
	if r1.RoundID != r2.RoundID {
		t.Fatalf("replay produced a different round: %s != %s", r1.RoundID, r2.RoundID)
	}
}

func TestSameKeyDifferentBetRejected(t *testing.T) {
	s := newSetup(t, nil, 1)
	if _, err := s.engine.Execute(ctx, req(nil)); err != nil {
		t.Fatal(err)
	}
	_, err := s.engine.Execute(ctx, req(func(r *round.Request) { r.Bet.Stake = usdt(2_000_000) }))
	if !errors.Is(err, round.ErrReplayMismatch) {
		t.Fatalf("want ErrReplayMismatch, got %v", err)
	}
}

func TestDebitDeclinePropagatesAndMovesNoMoney(t *testing.T) {
	s := newSetup(t, nil, 1)
	before := s.wallet.Balance("p", asset.USDT_TRON).Value()
	_, err := s.engine.Execute(ctx, req(func(r *round.Request) { r.Bet.Stake = usdt(999_000_000) }))
	var declined *seamless.DeclinedError
	if !errors.As(err, &declined) {
		t.Fatalf("want DeclinedError, got %v", err)
	}
	if s.wallet.Balance("p", asset.USDT_TRON).Value().Cmp(before) != 0 {
		t.Fatal("a declined debit moved money")
	}
}

func TestSettlementFailureAfterDebitRollsBack(t *testing.T) {
	w := seamless.NewInMemory(seamless.Opts{})
	w.Fund("p", usdt(100_000_000))
	reg := tenant.NewRegistry()
	tc, _ := tenant.NewConfig(tenant.Spec{
		OperatorID: "op1", EnabledGames: []string{"faulty"},
		AllowedAssets: []string{usdtKey}, CallbackSecret: "s",
	})
	_ = reg.Register(tc, w)
	e := round.NewEngine(round.Deps{
		Games: map[string]game.Game{"faulty": faulty{}}, Registry: reg, Ledger: ledger.New(), CreditRetries: 1,
	})
	before := w.Balance("p", asset.USDT_TRON).Value()
	_, err := e.Execute(ctx, req(func(r *round.Request) {
		r.GameName = "faulty"
		r.IdempotencyKey = "rb"
	}))
	if err == nil || err.Error() != "settlement fault" {
		t.Fatalf("want settlement fault, got %v", err)
	}
	if w.Balance("p", asset.USDT_TRON).Value().Cmp(before) != 0 {
		t.Fatal("stake was not restored by rollback")
	}
}

func TestCreditFailureMarksPayoutPending(t *testing.T) {
	fail := true
	s := newSetup(t, func(op string) error {
		if op == "credit" && fail {
			fail = false
			return &seamless.UnavailableError{Op: "credit", Msg: "wallet timeout"}
		}
		return nil
	}, 0) // no in-band retry so the single faulted credit is observable
	var pending *round.Result
	for i := 0; i < 20 && pending == nil; i++ {
		r, err := s.engine.Execute(ctx, req(func(r *round.Request) {
			r.IdempotencyKey = "c-" + string(rune('a'+i))
			r.Bet.Target = 1 // deterministic win -> a payout is always attempted
		}))
		if err != nil {
			t.Fatal(err)
		}
		if r.Outcome.Won && r.Settlement == round.SettlementPending {
			rc := r
			pending = &rc
		}
	}
	if pending == nil {
		t.Fatal("a winning round whose credit faulted should be payout_pending")
	}
	if pending.PayoutTxRef == "" {
		t.Fatal("pending payout must expose its txRef for re-credit")
	}
	if s.wallet.Balance("p", asset.USDT_TRON).Value().Sign() < 0 {
		t.Fatal("balance must never go negative")
	}
}

func TestConcurrentExecutesPreserveInvariants(t *testing.T) {
	s := newSetup(t, nil, 1) // p funded with 100 USDT
	// Two 60-USDT stakes against a 100-USDT balance: at most one can clear on
	// debit alone, so a naive engine would overdraw. The per-account lock plus
	// the wallet's debit check together prevent it.
	stake := usdt(60_000_000)
	var wg sync.WaitGroup
	for _, key := range []string{"a", "b"} {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			_, _ = s.engine.Execute(ctx, req(func(r *round.Request) {
				r.IdempotencyKey = k
				r.Bet.Stake = stake
			}))
		}(key)
	}
	wg.Wait()
	if s.wallet.Balance("p", asset.USDT_TRON).Value().Sign() < 0 {
		t.Fatal("balance went negative under concurrency")
	}
	if err := s.ledger.Verify(); err != nil {
		t.Fatalf("ledger integrity broken under concurrency: %v", err)
	}
}

// faulty is a duck-typed game whose Settle faults after the debit, exercising
// the rollback path.
type faulty struct{}

func (faulty) Name() string                 { return "faulty" }
func (faulty) ValidateBet(_ game.Bet) error { return nil }
func (faulty) Settle(_ game.Bet, _ rng.Roll) (game.Result, error) {
	return game.Result{}, errors.New("settlement fault")
}
