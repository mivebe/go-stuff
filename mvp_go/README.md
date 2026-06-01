# mvp_go

A crypto **game-provider (RGS) engine** in Go, in `roundrunner/`. It is the Go
counterpart of `../mvp_js/crypto-roundrunner`: the same money loop, crypto-shaped
(BigInt/multi-asset money via `math/big`) and multi-tenant (one engine serves
many operators over a seamless-wallet boundary; the operator custodies funds, the
provider never does).

## Docs

- `roundrunner/README.md` — the project: how to run it, architecture, what it
  covers, deliberate simplifications.
- `roundrunner/PLATFORM_PRIMER.md` — the inversion from custodial casino to game
  provider: seamless wallet, multi-tenancy, the re-drawn round lifecycle, and the
  Go-vs-JS differences.
- `roundrunner/EDGE_CASES.md` — a field guide to what bites you (Go number
  precision, idempotency, concurrency, confirmations, provable-fairness attacks,
  the seamless boundary, regulatory hooks, observability).
- `DOMAIN_PRIMER.md` — the underlying real-money-gaming fundamentals
  (determinism, idempotency, auditability, integer money) and vocabulary.

## Run it

```bash
cd roundrunner
go run ./cmd/roundrunner   # two-operator self-narrating simulation
go test ./...              # the invariants as contract-style tests
go run ./cmd/roundrunner -serve   # tenant-aware REST surface
```

## How it relates to the JS sibling

`../mvp_js/crypto-roundrunner` is the reference. This Go port matches its
behaviour — multi-asset BigInt money, multi-tenant operator registry, seamless
wallet (debit/credit/rollback + fault injection), per-asset double-entry ledger,
rollback & payout-pending paths, tenant-scoped idempotency, per-operator GGR,
provable fairness, RTP convergence — with idiomatic Go substitutions (`math/big`
for `BigInt`, `sync.Mutex` per account for the JS `AsyncMutex`, `error` returns
for thrown exceptions). The differences are catalogued in
`roundrunner/PLATFORM_PRIMER.md` §6.
