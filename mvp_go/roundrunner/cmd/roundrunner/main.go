// Command roundrunner demonstrates the game-provider engine. By default it runs
// an offline, self-narrating simulation across TWO operators (tenants) that
// exercises every property the engine guarantees and prints the evidence. Pass
// -serve to expose the tenant-aware REST API instead.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"

	"roundrunner/internal/api"
	"roundrunner/internal/asset"
	"roundrunner/internal/game"
	"roundrunner/internal/ledger"
	"roundrunner/internal/rng"
	"roundrunner/internal/round"
	"roundrunner/internal/seamless"
	"roundrunner/internal/tenant"
)

func main() {
	serve := flag.Bool("serve", false, "run the REST server instead of the simulation")
	addr := flag.String("addr", ":8080", "address for the REST server")
	flag.Parse()

	if *serve {
		runServer(*addr)
		return
	}
	runSimulation()
}

// dice builds the platform's one game: dice-over on USDT (Tron), 2% house edge.
func dice() game.Config {
	cfg, err := game.NewConfig(game.Spec{
		Name:         "dice-over",
		Asset:        asset.USDT_TRON,
		HouseEdgeBps: 200,                                          // 2% house edge -> 98% RTP
		MinBet:       asset.NewInt(asset.USDT_TRON, 100_000),       // 0.10 USDT
		MaxBet:       asset.NewInt(asset.USDT_TRON, 1_000_000_000), // 1000.00 USDT
	})
	if err != nil {
		log.Fatal(err)
	}
	return cfg
}

func mustTenant(s tenant.Spec) tenant.Config {
	c, err := tenant.NewConfig(s)
	if err != nil {
		log.Fatal(err)
	}
	return c
}

func runServer(addr string) {
	d := dice()
	games := map[string]game.Game{d.Name(): d}
	registry := tenant.NewRegistry()
	w := seamless.NewInMemory(seamless.Opts{})
	w.Fund("alice", asset.NewInt(asset.USDT_TRON, 1_000_000_000))
	_ = registry.Register(mustTenant(tenant.Spec{
		OperatorID:     "acme",
		EnabledGames:   []string{"dice-over"},
		AllowedAssets:  []string{asset.USDT_TRON.Key()},
		CallbackSecret: "acme-secret-key",
		Jurisdiction:   "curacao",
	}), w)

	l := ledger.New()
	engine := round.NewEngine(round.Deps{Games: games, Registry: registry, Ledger: l, CreditRetries: 1})
	srv := api.NewServer(engine, l)
	log.Printf("listening on %s  (POST /v1/rounds, GET /v1/ledger)", addr)
	log.Fatal(http.ListenAndServe(addr, srv.Routes()))
}

func runSimulation() {
	ctx := context.Background()
	usdt := asset.USDT_TRON
	d := dice()
	games := map[string]game.Game{d.Name(): d}

	// --- Two operators (tenants), each with its own wallet, secret, limits ---
	registry := tenant.NewRegistry()

	acmeWallet := seamless.NewInMemory(seamless.Opts{})
	_ = registry.Register(mustTenant(tenant.Spec{
		OperatorID:     "acme",
		DisplayName:    "Acme Casino",
		EnabledGames:   []string{"dice-over"},
		AllowedAssets:  []string{usdt.Key()},
		CallbackSecret: "acme-secret-key",
		Jurisdiction:   "curacao",
	}), acmeWallet)

	betcoWallet := seamless.NewInMemory(seamless.Opts{})
	_ = registry.Register(mustTenant(tenant.Spec{
		OperatorID:    "betco",
		DisplayName:   "BetCo",
		EnabledGames:  []string{"dice-over"},
		AllowedAssets: []string{usdt.Key()},
		// BetCo runs a tighter per-operator max than the game default.
		Limits:         map[string]tenant.Limit{usdt.Key(): {Min: big.NewInt(100_000), Max: big.NewInt(50_000_000)}},
		CallbackSecret: "betco-secret-key",
		Jurisdiction:   "mga",
	}), betcoWallet)

	l := ledger.New()
	engine := round.NewEngine(round.Deps{Games: games, Registry: registry, Ledger: l, CreditRetries: 1})

	// Operator-side balances (the operator custodies these; we never do).
	acmeWallet.Fund("alice", asset.NewInt(usdt, 1_000_000_000))  // 1000 USDT
	betcoWallet.Fund("alice", asset.NewInt(usdt, 1_000_000_000)) // a DIFFERENT alice

	stake := asset.NewInt(usdt, 10_000_000) // 10 USDT

	// --- Tenant isolation: same idempotency key + same player name, two operators ---
	fmt.Println("== tenant isolation (same key 'k1', same player name 'alice') ==")
	for _, op := range []string{"acme", "betco"} {
		res, err := engine.Execute(ctx, round.Request{
			OperatorID:     op,
			PlayerID:       "alice",
			GameName:       "dice-over",
			ClientSeed:     "alice-seed",
			IdempotencyKey: "k1",
			Bet:            game.Bet{Stake: stake, Target: 5000},
		})
		if err != nil {
			log.Fatal(err)
		}
		status := "LOST"
		if res.Outcome.Won {
			status = "WON "
		}
		fmt.Printf("%-6s round=%-18s roll=%4d %s settlement=%s\n",
			op, res.RoundID, res.Outcome.Roll, status, res.Settlement)
	}
	fmt.Printf("acme alice wallet:  %s\nbetco alice wallet: %s  (different person, independent balance)\n\n",
		acmeWallet.Balance("alice", usdt), betcoWallet.Balance("alice", usdt))

	// --- Unknown operator fails closed ---
	fmt.Println("== unknown operator rejected ==")
	_, err := engine.Execute(ctx, round.Request{
		OperatorID: "ghost", PlayerID: "x", GameName: "dice-over",
		ClientSeed: "s", IdempotencyKey: "k", Bet: game.Bet{Stake: stake, Target: 5000},
	})
	var unknownOp *tenant.UnknownOperatorError
	if errors.As(err, &unknownOp) {
		fmt.Printf("rejected: %s\n\n", err)
	} else {
		fmt.Printf("?: %v\n\n", err)
	}

	// --- Idempotent replay is tenant-scoped ---
	fmt.Println("== idempotent replay (acme / k1) ==")
	before := acmeWallet.Balance("alice", usdt).Value()
	replay, _ := engine.Execute(ctx, round.Request{
		OperatorID: "acme", PlayerID: "alice", GameName: "dice-over",
		ClientSeed: "alice-seed", IdempotencyKey: "k1", Bet: game.Bet{Stake: stake, Target: 5000},
	})
	unchanged := acmeWallet.Balance("alice", usdt).Value().Cmp(before) == 0
	fmt.Printf("balance unchanged after replay? %v   (stable round id: %s)\n\n", unchanged, replay.RoundID)

	// --- Provable fairness: verify the replayed round from the revealed seed ---
	fmt.Println("== provable fairness (verify acme/k1) ==")
	seedBytes, _ := hex.DecodeString(replay.ServerSeed)
	ok := rng.Verify(replay.Commitment, seedBytes, replay.ClientSeed, replay.Nonce, replay.Outcome.Roll)
	fmt.Printf("revealed seed recomputes the roll? %v\n\n", ok)

	// --- Rollback: a debit succeeds, settlement faults -> stake is restored ---
	fmt.Println("== rollback on post-debit failure (acme) ==")
	rbWallet := seamless.NewInMemory(seamless.Opts{})
	rbWallet.Fund("alice", asset.NewInt(usdt, 100_000_000)) // 100 USDT
	balPreRollback := rbWallet.Balance("alice", usdt).Value()
	rbRegistry := tenant.NewRegistry()
	_ = rbRegistry.Register(mustTenant(tenant.Spec{
		OperatorID: "acme", EnabledGames: []string{"faulty-dice"},
		AllowedAssets: []string{usdt.Key()}, CallbackSecret: "k",
	}), rbWallet)
	rbEngine := round.NewEngine(round.Deps{
		Games:    map[string]game.Game{"faulty-dice": faultyGame{name: "faulty-dice"}},
		Registry: rbRegistry, Ledger: ledger.New(), CreditRetries: 1,
	})
	_, rbErr := rbEngine.Execute(ctx, round.Request{
		OperatorID: "acme", PlayerID: "alice", GameName: "faulty-dice",
		ClientSeed: "alice-seed", IdempotencyKey: "rollback-1", Bet: game.Bet{Stake: stake, Target: 5000},
	})
	fmt.Printf("round aborted: %v\n", rbErr)
	restored := rbWallet.Balance("alice", usdt).Value().Cmp(balPreRollback) == 0
	fmt.Printf("stake rolled back? %v   (balance restored to pre-bet)\n\n", restored)

	// --- Payout pending: credit leg faults once -> recorded, not rolled back ---
	fmt.Println("== payout-pending (betco, first credit leg faults) ==")
	failNextCredit := true
	betcoFaulty := seamless.NewInMemory(seamless.Opts{
		Faults: func(op string) error {
			if op == "credit" && failNextCredit {
				failNextCredit = false
				return &seamless.UnavailableError{Op: "credit", Msg: "operator wallet timeout"}
			}
			return nil
		},
	})
	betcoFaulty.Fund("bob", asset.NewInt(usdt, 1_000_000_000))
	reg2 := tenant.NewRegistry()
	_ = reg2.Register(mustTenant(tenant.Spec{
		OperatorID: "betco", EnabledGames: []string{"dice-over"},
		AllowedAssets: []string{usdt.Key()}, CallbackSecret: "betco-secret-key",
	}), betcoFaulty)
	engine2 := round.NewEngine(round.Deps{
		Games: games, Registry: reg2, Ledger: ledger.New(), CreditRetries: 0, // no in-band retry
	})
	for i := 0; i < 12; i++ {
		res, err := engine2.Execute(ctx, round.Request{
			OperatorID: "betco", PlayerID: "bob", GameName: "dice-over",
			ClientSeed: "bob-seed", IdempotencyKey: fmt.Sprintf("payout-%d", i),
			Bet: game.Bet{Stake: stake, Target: 100}, // ~99% win chance
		})
		if err != nil {
			log.Fatal(err)
		}
		if res.Outcome.Won {
			line := fmt.Sprintf("round %s WON, settlement=%s", res.RoundID, res.Settlement)
			if res.Settlement == round.SettlementPending {
				line += fmt.Sprintf("  -> owed, re-credit via txRef %s (NOT rolled back)", res.PayoutTxRef)
				fmt.Println(line)
				break
			}
		}
	}
	fmt.Println()

	// --- Per-operator GGR off the reconciliation ledger ---
	fmt.Println("== per-operator GGR (provider reconciliation mirror) ==")
	for _, op := range []string{"acme", "betco"} {
		ggr := l.Balance(fmt.Sprintf("op:%s:ggr", op), usdt)
		fmt.Printf("%-6s GGR = %s  (stakes - payouts; revenue-share basis)\n", op, ggr)
	}
	if err := l.Verify(); err != nil {
		fmt.Printf("ledger verify FAILED: %v\n", err)
	} else {
		fmt.Printf("\nledger hash chain intact across %d records\n", len(l.Records()))
	}

	// --- RTP convergence for one operator (proves the game math pre-launch) ---
	fmt.Println("\n== RTP convergence (acme, 50k rounds) ==")
	rtpWallet := seamless.NewInMemory(seamless.Opts{})
	rtpReg := tenant.NewRegistry()
	_ = rtpReg.Register(mustTenant(tenant.Spec{
		OperatorID: "acme", EnabledGames: []string{"dice-over"},
		AllowedAssets: []string{usdt.Key()}, CallbackSecret: "k",
	}), rtpWallet)
	rtpEngine := round.NewEngine(round.Deps{Games: games, Registry: rtpReg, Ledger: ledger.New(), CreditRetries: 1})
	rtpWallet.Fund("bot", asset.NewInt(usdt, 1_000_000_000_000)) // 1,000,000 USDT
	stake1 := asset.NewInt(usdt, 1_000_000)                      // 1 USDT
	staked := new(big.Int)
	returned := new(big.Int)
	const n = 50_000
	for i := 0; i < n; i++ {
		res, err := rtpEngine.Execute(ctx, round.Request{
			OperatorID: "acme", PlayerID: "bot", GameName: "dice-over",
			ClientSeed: "bot-seed", IdempotencyKey: fmt.Sprintf("bot-%d", i),
			Bet: game.Bet{Stake: stake1, Target: 5000},
		})
		if err != nil {
			log.Fatal(err)
		}
		staked.Add(staked, stake1.Value())
		returned.Add(returned, res.Outcome.Payout.Value())
	}
	ratio := new(big.Float).Quo(new(big.Float).SetInt(returned), new(big.Float).SetInt(staked))
	pct, _ := ratio.Float64()
	fmt.Printf("staked=%s returned=%s realised RTP=%.2f%% (target 98.00%%)\n",
		staked.String(), returned.String(), pct*100)
}

// faultyGame is a duck-typed game whose Settle faults AFTER the debit, used to
// exercise the engine's rollback path. It satisfies game.Game.
type faultyGame struct{ name string }

func (f faultyGame) Name() string                 { return f.name }
func (f faultyGame) ValidateBet(_ game.Bet) error { return nil }
func (f faultyGame) Settle(_ game.Bet, _ rng.Roll) (game.Result, error) {
	return game.Result{}, errors.New("settlement engine fault")
}
