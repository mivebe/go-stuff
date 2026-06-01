# crypto-roundrunner

The same round engine as `roundrunner` (Go), rewritten in modern JavaScript
and adapted for a crypto casino + crypto wallet product. Pure Node.js standard
library — no dependencies.

The companion document is `EDGE_CASES.md` — a field guide to the things that
bite you in this domain, grouped by theme and tied back to the code.

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
  asset.js   # Asset, Amount (BigInt-only), well-known crypto assets
  rng.js     # commit-reveal provably-fair outcome generation
  ledger.js  # append-only, hash-chained, multi-asset double-entry
  wallet.js  # in-memory crypto wallet (pending/confirmed, multi-asset)
  game.js    # config + pure outcome/payout settlement
  round.js   # stateless idempotent runtime + AsyncMutex
  main.js    # demo: deposit lifecycle, rounds, idempotency, fairness, RTP
tests/
  amount.test.js   # BigInt arithmetic, asset-mismatch, mulBps, JSON roundtrip
  ledger.test.js   # per-asset balance, multi-asset, tamper detection
  round.test.js    # idempotent replay, replay mismatch, concurrency invariants
EDGE_CASES.md      # the field guide — what bites you, grouped
```

## Deliberate simplifications

- **In-memory state.** Wallet, ledger, idempotency store are all maps. In
  production each is a database; the round's mutex becomes a single DB
  transaction.
- **Global mutex.** Locking is per-engine, not per-account. Rounds for
  different players should be able to run in parallel; production uses a
  per-(account, asset) lock or DB row locks.
- **No reorg handling.** Deposits go pending → confirmed and stay there. A
  real implementation listens for reorgs and writes reversal journals when
  a confirmed deposit gets unwound.
- **No regulatory hooks.** Self-exclusion, deposit limits, KYC tiers, KYT
  screening — all listed in `EDGE_CASES.md` §11 as the integration points
  before the bet reaches the engine.
- **No address validation or withdrawal flow.** Out of scope for the engine
  itself; covered conceptually in `EDGE_CASES.md` §9.
