package round_test

import (
	"errors"
	"math/big"
	"testing"

	"roundrunner/internal/asset"
	"roundrunner/internal/game"
	"roundrunner/internal/ledger"
	"roundrunner/internal/round"
	"roundrunner/internal/seamless"
	"roundrunner/internal/tenant"
)

// platform builds two operators sharing one engine, each with its own wallet.
func platform(t *testing.T) (*round.Engine, *ledger.Ledger, map[string]*seamless.InMemoryOperatorWallet) {
	t.Helper()
	g, err := game.NewConfig(game.Spec{
		Name: "dice", Asset: asset.USDT_TRON, HouseEdgeBps: 200,
		MinBet: usdt(1), MaxBet: usdt(1_000_000_000),
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := tenant.NewRegistry()
	wallets := map[string]*seamless.InMemoryOperatorWallet{}
	for _, id := range []string{"acme", "betco"} {
		w := seamless.NewInMemory(seamless.Opts{})
		w.Fund("alice", usdt(100_000_000))
		wallets[id] = w
		spec := tenant.Spec{
			OperatorID: id, EnabledGames: []string{"dice"},
			AllowedAssets: []string{usdtKey}, CallbackSecret: id + "-secret",
		}
		if id == "betco" { // betco runs a tighter max
			spec.Limits = map[string]tenant.Limit{usdtKey: {Min: big.NewInt(1), Max: big.NewInt(5_000_000)}}
		}
		tc, err := tenant.NewConfig(spec)
		if err != nil {
			t.Fatal(err)
		}
		if err := reg.Register(tc, w); err != nil {
			t.Fatal(err)
		}
	}
	l := ledger.New()
	return round.NewEngine(round.Deps{
		Games: map[string]game.Game{"dice": g}, Registry: reg, Ledger: l, CreditRetries: 1,
	}), l, wallets
}

func opReq(operatorID string, over func(*round.Request)) round.Request {
	r := round.Request{
		OperatorID: operatorID, PlayerID: "alice", GameName: "dice",
		ClientSeed: "s", IdempotencyKey: "k1",
		Bet: game.Bet{Stake: usdt(1_000_000), Target: 5000},
	}
	if over != nil {
		over(&r)
	}
	return r
}

func TestUnknownOperatorFailsClosed(t *testing.T) {
	e, _, wallets := platform(t)
	before := wallets["acme"].Balance("alice", asset.USDT_TRON).Value()
	_, err := e.Execute(ctx, opReq("ghost", nil))
	var unknown *tenant.UnknownOperatorError
	if !errors.As(err, &unknown) {
		t.Fatalf("want UnknownOperatorError, got %v", err)
	}
	if wallets["acme"].Balance("alice", asset.USDT_TRON).Value().Cmp(before) != 0 {
		t.Fatal("an unknown operator touched money")
	}
}

func TestIdempotencyIsTenantScoped(t *testing.T) {
	e, _, _ := platform(t)
	a, err := e.Execute(ctx, opReq("acme", nil)) // key k1
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Execute(ctx, opReq("betco", nil)) // SAME key k1, different tenant
	if err != nil {
		t.Fatal(err)
	}
	if a.RoundID == b.RoundID {
		t.Fatal("idempotency keys must not collide across tenants")
	}
	if a.OperatorID != "acme" || b.OperatorID != "betco" {
		t.Fatal("operator ids mismatched")
	}
}

func TestPlayerNameIsPerOperator(t *testing.T) {
	e, _, wallets := platform(t)
	if _, err := e.Execute(ctx, opReq("acme", nil)); err != nil {
		t.Fatal(err)
	}
	if wallets["acme"].Balance("alice", asset.USDT_TRON).Value().Cmp(big.NewInt(100_000_000)) == 0 {
		t.Fatal("acme alice's balance should have changed")
	}
	if wallets["betco"].Balance("alice", asset.USDT_TRON).Value().Cmp(big.NewInt(100_000_000)) != 0 {
		t.Fatal("betco alice (a different person) must be untouched")
	}
}

func TestGameNotEnabledRejected(t *testing.T) {
	g, _ := game.NewConfig(game.Spec{
		Name: "dice", Asset: asset.USDT_TRON, HouseEdgeBps: 200,
		MinBet: usdt(1), MaxBet: usdt(1_000_000_000),
	})
	reg := tenant.NewRegistry()
	w := seamless.NewInMemory(seamless.Opts{})
	w.Fund("alice", usdt(100_000_000))
	tc, _ := tenant.NewConfig(tenant.Spec{
		OperatorID: "acme", EnabledGames: []string{"some-other-game"},
		AllowedAssets: []string{usdtKey}, CallbackSecret: "s",
	})
	_ = reg.Register(tc, w)
	e := round.NewEngine(round.Deps{Games: map[string]game.Game{"dice": g}, Registry: reg, Ledger: ledger.New()})
	_, err := e.Execute(ctx, opReq("acme", nil))
	var off *round.GameNotEnabledError
	if !errors.As(err, &off) {
		t.Fatalf("want GameNotEnabledError, got %v", err)
	}
}

func TestAssetNotAllowedRejected(t *testing.T) {
	e, _, _ := platform(t)
	_, err := e.Execute(ctx, opReq("acme", func(r *round.Request) {
		r.Bet.Stake = asset.NewInt(asset.BTC, 1000)
	}))
	var off *round.AssetNotAllowedError
	if !errors.As(err, &off) {
		t.Fatalf("want AssetNotAllowedError, got %v", err)
	}
}

func TestPerOperatorStakeLimitEnforced(t *testing.T) {
	e, _, _ := platform(t)
	// betco caps stakes at 5 USDT; 10 USDT is within the game default but over
	// betco's tenant limit.
	_, err := e.Execute(ctx, opReq("betco", func(r *round.Request) { r.Bet.Stake = usdt(10_000_000) }))
	var lim *round.LimitError
	if !errors.As(err, &lim) {
		t.Fatalf("want LimitError, got %v", err)
	}
}

func TestGGRTrackedPerOperator(t *testing.T) {
	e, l, _ := platform(t)
	if _, err := e.Execute(ctx, opReq("acme", func(r *round.Request) { r.IdempotencyKey = "a1" })); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(ctx, opReq("betco", func(r *round.Request) { r.IdempotencyKey = "b1" })); err != nil {
		t.Fatal(err)
	}
	// For each operator the GGR pool is exactly the negation of the player
	// mirror — the per-operator invoicing basis.
	for _, op := range []string{"acme", "betco"} {
		ggr := l.Balance("op:"+op+":ggr", asset.USDT_TRON).Value()
		player := l.Balance("op:"+op+":player:alice", asset.USDT_TRON).Value()
		if new(big.Int).Add(ggr, player).Sign() != 0 {
			t.Fatalf("%s GGR does not mirror its player postings", op)
		}
	}
	if l.Balance("op:acme:player:alice", asset.USDT_TRON).Value().Sign() == 0 {
		t.Fatal("acme should carry its own round footprint")
	}
	if err := l.Verify(); err != nil {
		t.Fatal(err)
	}
}
