# crypto-roundrunner

The same round engine as `roundrunner` (Go), rewritten in modern JavaScript and
adapted for a crypto casino domain. Pure Node.js standard library — no
dependencies.

It now runs as a **multi-tenant slot game provider platform** (an RGS — Remote
Game Server): one engine serving many **operators** (casino brands) as tenants,
integrating with each operator's wallet over a **seamless wallet** boundary
rather than custodying funds itself.

Companion documents:

- `PLATFORM_PRIMER.md` — the provider-platform architecture: the custodial →
  provider inversion, the seamless wallet model, multi-tenancy, and the
  re-drawn round lifecycle.
- `EDGE_CASES.md` — a field guide to what bites you in this domain (§13
  multi-tenant isolation, §14 the seamless wallet boundary are the platform
  additions).

Read both alongside the code: concept there, code here.

## Run it

Requires Node 20+.

```bash
node src/main.js              # self-narrating simulation
node --test tests/*.test.js   # invariant tests
```

## What's different from the Go version

The Go version was minor-units-in-`int64` and single-asset. This one is
crypto-shaped:

- **`BigInt` end to end.** 1 ETH in base units (wei) is `10^18`, well past
  `Number.MAX_SAFE_INTEGER`. Using `Number` for money silently loses
  precision; `BigInt` is non-negotiable.
- **Multi-asset by construction.** Every `Amount` is tagged with its `Asset`,
  and asset identity is `(symbol, chain)` — USDT-on-Tron and USDT-on-Ethereum
  are different assets. Cross-asset arithmetic throws at runtime.
- **Per-asset double-entry.** The ledger requires balances per asset to net
  to zero in every journal, not just in aggregate. A BTC debit cannot be
  cancelled by a USDT credit.
- **Pending vs confirmed balances.** Deposits go through an N-confirmation
  threshold before becoming bettable; the wallet exposes both `pendingFor`
  and `balance`.
- **Async wallet boundary.** Wallet ops are `async` to model real network
  calls to a wallet service. An `AsyncMutex` in the engine stands in for the
  database transaction that wraps the money loop in production.

## Layout

```
src/
  asset.js     # Asset, Amount (BigInt-only), well-known crypto assets
  rng.js       # commit-reveal provably-fair outcome generation
  ledger.js    # append-only, hash-chained, multi-asset double-entry (now a
               #   per-operator GGR reconciliation mirror, not custody)
  game.js      # config + pure outcome/payout settlement (the platform's games)
  tenant.js    # TenantConfig + OperatorRegistry — the multi-tenant boundary
  seamless.js  # seamless wallet contract + in-memory operator-wallet fake
  round.js     # tenant-aware idempotent runtime; calls the seamless wallet
  wallet.js    # legacy custodial in-memory wallet (kept as a reference; the
               #   operator now owns custody behind the seamless boundary)
  main.js      # demo: two operators, isolation, rollback, payout-pending, GGR
tests/
  amount.test.js   # BigInt arithmetic, asset-mismatch, mulBps, JSON roundtrip
  ledger.test.js   # per-asset balance, multi-asset, tamper detection
  round.test.js    # idempotent replay, decline, rollback, payout-pending
  tenant.test.js   # unknown-operator, tenant-scoped idempotency, isolation, GGR
PLATFORM_PRIMER.md # the provider-platform architecture
EDGE_CASES.md      # the field guide — what bites you, grouped
```

## Deliberate simplifications

- **In-memory state.** Operator wallets, ledger, idempotency store are all maps.
  In production each is a database (the operator's wallet is a remote service);
  the per-account lock becomes a single DB transaction.
- **Per-engine round-id sequence.** Round ids use a per-engine counter, unique
  only within one engine instance. Production needs a globally-unique source
  (UUID or DB sequence) so `txRef`s never collide across instances.
- **Seamless wallet is in-process.** `InMemoryOperatorWallet` stands in for a
  real operator wallet API (HTTP/gRPC). It models the two things that make the
  boundary hard — `txRef` idempotency and injectable failures — but not auth,
  retries with backoff, or network latency.
- **No reconciliation job.** Orphaned debits and pending payouts are surfaced
  (`settlement: "payout_pending"`) but not swept; `EDGE_CASES.md` §14 describes
  the periodic reconcile-against-the-operator job that closes them out.
- **No regulatory hooks at the provider.** Self-exclusion, deposit/loss limits,
  KYC tiers, KYT — these live on the **operator** side now (`EDGE_CASES.md`
  §11); the provider enforces only per-tenant game/asset enablement and bet
  limits.
