# roundrunner

A minimal but honest **crypto game-provider (RGS) engine** in Go. It is the Go
counterpart of `../../mvp_js/crypto-roundrunner`: the same money loop — outcome
generation, settlement, a tamper-evident double-entry ledger, idempotent
execution — but crypto-shaped (BigInt/multi-asset money) and multi-tenant
(serves many operators over a seamless-wallet boundary). It proves each property
when you run it.

It started life as a single-tenant **custodial** engine (the platform *was* the
casino, holding `int64` fiat balances). It is now a **game provider**: it never
custodies funds, calls each operator's wallet per money event, and treats its
own ledger as a per-operator GGR reconciliation mirror. See `PLATFORM_PRIMER.md`
for that inversion and `DOMAIN_PRIMER.md` for the underlying domain.

## Run it

No dependencies beyond the Go standard library (`math/big` for money).

```bash
# Self-narrating simulation: two operators, plays rounds, prints the evidence.
go run ./cmd/roundrunner

# Tests: the invariants, as contract-style tests.
go test ./...

# Tenant-aware REST server instead of the simulation.
go run ./cmd/roundrunner -serve
# POST a bet (stake is a base-units decimal STRING tagged with an asset key):
curl -s localhost:8080/v1/rounds -d '{"operator_id":"acme","player":"alice","game":"dice-over","client_seed":"hi","idempotency_key":"r1","asset":"USDT:tron","stake":"10000000","target":5000}'
# Inspect + verify the ledger:
curl -s localhost:8080/v1/ledger
```

The simulation demonstrates, in order: **tenant isolation** (same idempotency
key + same player name under two operators don't collide); an **unknown
operator** failing closed; a **tenant-scoped idempotent replay** that doesn't
double-charge; **provable-fairness** verification from a revealed seed; a
**rollback** when settlement faults after a debit; a **payout-pending** path
when the credit leg faults (recorded and owed, never rolled back); **per-operator
GGR** read off the reconciliation ledger; a **hash-chain integrity** check; and
**RTP convergence** over 50k rounds landing near the configured 98%.

## What's different from the original (fiat, single-tenant) engine

| Original | This engine |
| --- | --- |
| `int64` minor units | **`math/big.Int`** base units — 1 ETH is 10^18 wei; int64 overflows at ten ETH |
| Single currency | **Multi-asset** — every `Amount` is tagged with its `Asset`; identity is `(symbol, chain)`, so USDT-on-Tron ≠ USDT-on-Ethereum, and cross-asset arithmetic panics |
| Double-entry in aggregate | **Per-asset double-entry** — a BTC debit can't be cancelled by a USDT credit |
| One brand, you hold funds | **Multi-tenant** — one engine serves many operators; the **operator** custodies funds |
| In-process wallet | **Seamless wallet boundary** — `Debit`/`Credit`/`Rollback` to the operator, idempotent on a `TxRef` |
| Ledger = source of truth | Ledger = **GGR reconciliation mirror**, accounts namespaced per operator |
| Global mutex | **Per-(operator, player) lock**, so unrelated players don't serialise |
| Global nonce | **Per-(operator, player, clientSeed) seed session** |

## Architecture

The transport is a detail; the **engine** is the product. Dependencies flow
downward only (no cycles).

```
cmd/roundrunner       two-operator demo simulation + optional REST server
internal/api          thin tenant-aware REST surface (decode -> engine -> encode)
internal/round        tenant-aware, idempotent runtime — orchestrates one round
internal/game         config (economics) + pure outcome/payout settlement; Game interface
internal/tenant       operator registry + per-operator TenantConfig (fails closed)
internal/seamless     operator-wallet boundary: Debit/Credit/Rollback, idempotent on TxRef, fault injection
internal/cryptowallet custodial crypto wallet with a pending/confirmed deposit lifecycle (not used by the engine)
internal/rng          provably-fair commit-reveal outcome generation
internal/ledger       append-only, hash-chained, multi-asset (per-asset) double-entry
internal/asset        BigInt money (math/big) tagged with an Asset; well-known crypto assets
```

`round` depends on `game / rng / ledger / seamless / tenant / asset`; the lower
packages know nothing about it. `cryptowallet` is standalone — it captures the
pending/confirmed deposit story from the custodial design but the provider
engine calls the operator's seamless wallet instead.

## What it covers

| Concern | Where it lives |
| --- | --- |
| Tenant-aware gameplay runtime; stateless workers; round end-to-end | `internal/round` |
| Multi-tenancy: operators as tenants, fails closed on unknown operator | `internal/tenant` |
| Seamless wallet: debit/credit/rollback across a boundary, idempotent on `TxRef` | `internal/seamless` |
| Rollback on post-debit failure; payout-pending on credit failure | `round.Engine.Execute` |
| Tamper-evident, append-only, **per-asset** double-entry ledger | `internal/ledger` (hash chain) |
| BigInt multi-asset money; never floats; asset-tagged | `internal/asset` (`math/big`) |
| Outcome generation; commit-reveal / provably verifiable | `internal/rng` (`Commit`, `Generate`, `Verify`) |
| Game config: economics, bet limits, asset | `internal/game` (`Config`) |
| Simulation/verification that proves game logic | RTP convergence in `cmd/roundrunner`; pure `Settle` |
| Pending/confirmed crypto deposits (custodial story) | `internal/cryptowallet` |
| Per-operator GGR for revenue-share / invoicing | `ledger.Balance("op:<id>:ggr", asset)` |
| Client-facing surfaces (REST) | `internal/api` |

## Deliberate simplifications (and the production version)

This is an MVP; it names its own shortcuts so the production path is explicit.

- **In-memory state.** The wallet, ledger, and idempotency store are maps. In
  production they're a database; the round's critical section becomes one **DB
  transaction** for the provider's own state, while correctness *across* the
  seamless boundary is carried by `TxRef` idempotency and the explicit rollback
  path — you can't wrap someone else's wallet in your transaction.
- **Per-account mutex stands in for a transaction / row lock.** Real atomicity
  comes from the DB; the per-(operator, player) lock removes the global
  bottleneck.
- **Asset mismatch / negative `MulBps` panic.** They're programming bugs, so
  they fail loud (the JS sibling throws). Inputs are validated before they reach
  arithmetic.
- **Modulo bias in RNG mapping** is noted in `rng.Generate`; a stricter version
  uses rejection sampling.
- **No signing yet.** The ledger is tamper-*evident* via hashing; add per-record
  signatures for "signed audit logs" (authorship, not just integrity).
- **`cryptowallet` has no reorg handling.** Deposits go pending → confirmed and
  stay there; a real implementation writes a reversal journal on a reorg.
- **No regulatory hooks.** Self-exclusion, deposit limits, KYC tiers, KYT — the
  operator's concern in this topology; integration points are in `EDGE_CASES.md`.

## Good next extensions (in rough order)

1. Add a second game type / RTP variant to exercise the config abstraction.
2. Define the round service as a `.proto` and add a **gRPC** surface over the
   same engine.
3. Add a **WebSocket** endpoint streaming round results.
4. Swap the in-memory seamless wallet for a real HTTP/gRPC operator adapter, with
   `TxRef` idempotency enforced by a unique constraint; make the provider's own
   state a real DB transaction.
5. Verify the seamless calls with the per-operator `CallbackSecret` (HMAC).
6. Emit metrics (per-round latency, debit/credit failures, idempotency-hit rate,
   ReplayMismatch rate, realised-vs-configured RTP drift, per-operator GGR).
