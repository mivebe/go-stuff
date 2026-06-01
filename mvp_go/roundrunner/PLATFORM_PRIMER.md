# Crypto Game Provider Platform — Architecture Primer (Go)

`roundrunner` started life as a **single-tenant, custodial** engine: the
platform *was* the casino. It held the wallet, custodied player funds (in
`int64` minor units), ran the RNG, and settled rounds against its own balances.

This document describes the shift to what the product actually needs to be: a
**multi-tenant game provider platform** — an **RGS** (Remote Game Server) in
iGaming terms — that serves many **operators** (casino brands) over an
integration boundary, in a crypto-casino domain. It is the Go sibling of
`../../mvp_js/crypto-roundrunner`; read it alongside `DOMAIN_PRIMER.md`
(fundamentals) and `EDGE_CASES.md` (what bites you), and the code: concept here,
code there.

---

## 1. The inversion: who are you, and who owns the money?

The single most important shift is *whose money it is*.

| | Custodial casino (before) | Game provider / RGS (now) |
|---|---|---|
| You are | the casino | a **game supplier** to casinos |
| Tenant | one brand | **many operators** |
| Player belongs to | you | the **operator** |
| Who holds funds | you (custody) | the **operator** (you never touch custody) |
| Wallet | yours, in-process | the operator's, **over an API** |
| Your ledger is | the source of truth for balances | a **reconciliation / GGR mirror** |
| Your liability | player balances | ~nothing — you're not a custodian |

A game provider does **not** custody player funds. The operator runs KYC,
deposits, withdrawals, responsible-gaming limits, and the wallet. You run the
**games**: outcome generation, math/RTP, round lifecycle, provable fairness, and
the integration that moves money *on the operator's books* per bet.

That inversion is liberating (you shed the custody, AML, and withdrawal
machinery — it's the operator's problem now) and constraining (every bet now
depends on a **network call to someone else's wallet**, with all the failure
modes that implies).

---

## 2. The seamless wallet model (`internal/seamless`)

The industry-standard integration between a game provider and an operator is the
**seamless wallet** (also "transfer wallet"). The provider does not keep a copy
of the player's balance; on every money event it calls the operator's wallet API
in real time. Three operations form the contract (in Go, the
`seamless.OperatorWallet` interface):

1. **`Debit`** (bet / wager) — remove the stake from the player's operator-side
   balance. Returns the new balance, or **declines** (`*seamless.DeclinedError`,
   a normal rejection — no outcome is generated).
2. **`Credit`** (win / payout) — add winnings. Only on a win.
3. **`Rollback`** (cancel) — reverse a previously-successful `Debit` when the
   round cannot complete (settlement fault, crash, timeout with no result).

Every call carries a **`TxRef`** — a provider-generated, globally-unique
transaction reference. The operator **dedupes on `TxRef`**: a retried `Debit`
with the same ref is a no-op that returns the original result, never a second
charge. This is idempotency stretched across an inter-company network boundary,
where retries are not a possibility but a certainty.

```
provider (RGS)                         operator wallet (custodian)
   │  Debit{TxRef=r-42:bet, amount}       │
   ├─────────────────────────────────────▶│  balance -= stake   (dedupe on TxRef)
   │◀─────────────────────────────────────┤  { balance }
   │   ...generate outcome, settle...      │
   │  Credit{TxRef=r-42:win, payout}       │   (only if won)
   ├─────────────────────────────────────▶│  balance += payout  (dedupe on TxRef)

And the unhappy path — debit succeeded but the round can't finish:
   │  Debit{TxRef=r-43:bet}                │  balance -= stake
   │  ✗ settlement faults / crash          │
   │  Rollback{BetTxRef=r-43:bet}          │
   ├─────────────────────────────────────▶│  balance += stake (reverse)
```

`seamless.InMemoryOperatorWallet` stands in for a real operator's wallet service
(HTTP/gRPC in production) and models the two behaviours that make the boundary
hard: `TxRef` idempotency, and injectable faults (the `Faults` hook) so the
rollback and retry paths are exercised, not merely described.

---

## 3. Multi-tenancy: operators as tenants (`internal/tenant`)

"Multi-tenant toward the operators" means **one platform, many operator brands,
partitioned**. The tenant is the **operator**, not the player. A player only
exists *within* an operator's namespace — `alice` under `acme` and `alice` under
`betco` are unrelated people.

What must be partitioned per tenant:

- **Identity & routing.** Every request names an operator and is routed to *that*
  operator's wallet (the engine resolves `(tenant.Config, seamless.OperatorWallet)`
  via `tenant.Registry.Resolve`, or rejects an unknown operator).
- **Config.** Enabled games, allowed assets, per-asset bet limits, jurisdiction.
  (`tenant.Config`.)
- **Secrets.** Each operator has its own **callback secret** (HMAC key) used to
  sign/verify seamless calls. One operator's key must never validate another's.
- **Idempotency scope.** The dedup key is `(operatorId, idempotencyKey)`, never
  the bare key. Two operators can independently send key `"k1"` without colliding.
- **RNG seed sessions.** The provably-fair nonce sequence is per
  `(operator, player, clientSeed)` — never global, or outcomes would correlate
  across operators and break auditability.
- **Reconciliation / GGR.** Ledger accounts are namespaced by operator
  (`op:acme:ggr`, `op:betco:ggr`), so per-operator **GGR** (gross gaming revenue)
  — the basis for revenue-share / invoicing — is a direct sum over that
  operator's postings.

The engine depends on the small `tenant.Registry` abstraction: given an
operatorId, hand back that tenant's config and wallet, or reject it. Operator
quirks (wallet API dialect, auth, currency mapping) live in the **wallet adapter
at the edge**; the engine core stays operator-agnostic.

---

## 4. The round lifecycle, re-drawn for a provider (`internal/round`)

The six-step round from `DOMAIN_PRIMER.md` §1 still holds, but steps 2 and 6 —
debit and credit — now cross the seamless boundary, and a tenant-resolution step
is bolted on the front (`round.Engine.Execute`):

0. **Resolve tenant.** Look up `tenant.Config` and the wallet. Unknown operator →
   reject (`*tenant.UnknownOperatorError`).
1. **Validate**: game enabled for this tenant, asset allowed, stake within this
   tenant's per-asset limits, target in range.
2. **Publish commitment**, derive `roundID` and `TxRef`s, take the per-account
   lock (`(operator, player)`).
3. **`Debit`** the stake (`TxRef = roundID:bet`). Declined → round rejected, no
   outcome generated.
4. **Generate outcome** (per-session nonce) and **`Settle`** (pure).
5. **Record** the round to the provider reconciliation ledger (per-operator GGR
   postings). *If anything in 4–5 faults after a successful debit →*
   **`Rollback`** *the debit and abort.*
6. On a win, **`Credit`** the payout (`TxRef = roundID:win`). Credit is
   **retried, not rolled back** — the player won; the obligation stands until the
   operator confirms it (`SettlementPending` surfaces the owed `PayoutTxRef`).

The atomic unit is still "the whole money loop," but it now spans two systems.
In-process, the per-`(operator, player)` `sync.Mutex` (plus a DB transaction in
production) guards the provider's own state; correctness *across* the boundary is
carried by `TxRef` idempotency and the explicit rollback path, because you cannot
wrap someone else's wallet in your transaction.

---

## 5. What stays exactly the same (and why that's the point)

The engine survives this inversion with a small refactor because its core
invariants are **not** about custody:

- **Determinism & provable fairness** (`internal/rng`) — unchanged; if anything
  *more* important, because now an external operator and their players audit you.
- **BigInt money & asset tagging** (`internal/asset`) — unchanged; amounts cross
  the wallet boundary as asset-tagged `math/big.Int` values.
- **Pure, simulatable settlement** (`internal/game`) — unchanged; you still prove
  RTP before shipping.
- **Append-only hash-chained ledger** (`internal/ledger`) — unchanged mechanism,
  new *role* (reconciliation/GGR mirror, accounts namespaced by operator).
- **Idempotency discipline** (`internal/round`) — unchanged principle, now scoped
  per tenant and extended across the seamless boundary via `TxRef`.

The refactor is concentrated where the inversion bites: a tenant boundary in
front (`internal/tenant`), and a wallet that calls out instead of holding funds
(`internal/seamless`).

---

## 6. Go-vs-JS notes

The JS sibling and this Go port make the same architectural choices; the
language differences worth calling out:

- **Money.** JS uses `BigInt`; Go uses `math/big.Int`. Both because crypto
  amounts (1 ETH = 10^18 wei) exceed the native exact-integer range (`Number`'s
  2^53, `int64`'s ~9.2 × 10^18).
- **Asset mismatch / negative `MulBps`.** JS throws; Go panics. Both treat them
  as programming bugs, not recoverable conditions.
- **Concurrency.** JS serialises with an `AsyncMutex` because every `await`
  yields; Go uses a real per-account `sync.Mutex` because goroutines are truly
  concurrent. Same role: a stand-in for a DB row lock / transaction.
- **Settlement fault.** JS models it with a thrown exception from `settle`; Go
  models it with the `error` return on the `game.Game` interface's `Settle`.
- **Nonce.** JS uses `BigInt` (its `Number` loses precision past 2^53); Go uses
  `uint64`, which holds ~1.8 × 10^19 rounds without loss.
