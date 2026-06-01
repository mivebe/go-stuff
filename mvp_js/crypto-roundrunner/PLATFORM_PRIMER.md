# Slot Game Provider Platform — Architecture Primer

`crypto-roundrunner` started life as a **single-tenant, custodial** engine: the
platform *was* the casino. It held the wallet, custodied player funds, ran the
RNG, and settled rounds against its own balances.

This document describes the shift to what the product actually needs to be: a
**multi-tenant slot game provider platform** — an **RGS** (Remote Game Server)
in iGaming terms — that serves many **operators** (casino brands) over an
integration boundary, in a crypto-casino domain.

Read it alongside `EDGE_CASES.md` (§13 multi-tenancy, §14 seamless wallet) and
the code: concept here, code there.

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
**games**: outcome generation, math/RTP, round lifecycle, provable fairness,
and the integration that moves money *on the operator's books* per bet.

That inversion is liberating (you shed the custody, AML, and withdrawal
machinery in `EDGE_CASES.md` §7–§9 — it's the operator's problem now) and
constraining (every bet now depends on a **network call to someone else's
wallet**, with all the failure modes that implies — §14).

---

## 2. The seamless wallet model (a.k.a. transfer wallet)

The industry-standard integration between a game provider and an operator is the
**seamless wallet** (also "transfer wallet"). The provider does not keep a copy
of the player's balance; on every money event it calls the operator's wallet
API in real time.

Three operations form the contract:

1. **`debit`** (bet / wager) — reserve and remove the stake from the player's
   operator-side balance. Returns the new balance, or **declines** (insufficient
   funds). *No outcome is generated until the debit succeeds.*
2. **`credit`** (win / payout) — add winnings to the player's operator-side
   balance. Only happens on a win.
3. **`rollback`** (cancel) — reverse a previously-successful `debit` when the
   round cannot complete (provider crash, settlement failure, timeout with no
   confirmed result). The operator restores the stake.

Every call carries a **`txRef`** — a provider-generated, globally-unique
transaction reference. The operator **dedupes on `txRef`**: a retried `debit`
with the same ref is a no-op that returns the original result, never a second
charge. This is idempotency (`EDGE_CASES.md` §3) stretched across an
inter-company network boundary, where retries are not a possibility but a
certainty.

```
provider (RGS)                         operator wallet (custodian)
   │   debit{txRef=r-42:bet, amount}      │
   ├─────────────────────────────────────▶│  balance -= stake   (dedupe on txRef)
   │◀─────────────────────────────────────┤  { balance }
   │   ...generate outcome, settle...      │
   │   credit{txRef=r-42:win, payout}      │   (only if won)
   ├─────────────────────────────────────▶│  balance += payout  (dedupe on txRef)
   │◀─────────────────────────────────────┤  { balance }
```

And the unhappy path — debit succeeded but the round can't finish:

```
   │   debit{txRef=r-43:bet}               │  balance -= stake
   │◀──────────────────────────────────────│
   │   ✗ settlement throws / crash          │
   │   rollback{betTxRef=r-43:bet}          │
   ├──────────────────────────────────────▶│  balance += stake (reverse)
```

> The alternative is a **transfer wallet** in the narrow sense, where the
> player moves a balance *into* the game session up front and *out* at the end
> (two transfers, gameplay against a provider-held session balance in between).
> It reduces per-spin chatter but reintroduces a provider-held balance and
> reconciliation on session boundaries. Seamless is the default and what this
> codebase models.

---

## 3. Multi-tenancy: operators as tenants

"Multi-tenant toward the operators" means **one platform, many operator
brands, partitioned**. The tenant is the **operator**, not the player. A player
only exists *within* an operator's namespace — `player:alice` under operator
`acme` and `player:alice` under operator `betco` are unrelated people.

What must be partitioned per tenant:

- **Identity & routing.** Every request is authenticated as an operator and
  routed to *that* operator's wallet endpoint and credentials.
- **Config.** Enabled games, allowed currencies/assets, per-currency bet
  limits, RTP variant, jurisdiction, branding. (`TenantConfig`.)
- **Secrets.** Each operator has its own **callback secret** (HMAC key) used to
  sign/verify the seamless calls. One operator's key must never validate
  another's traffic.
- **Idempotency scope.** Idempotency keys are namespaced **per operator**. Two
  operators can independently send key `"k1"` without colliding. The dedup
  store key is `(operatorId, idempotencyKey)`, never the bare key.
- **RNG seed sessions.** The provably-fair nonce sequence is per
  `(operator, player, clientSeed)` session — never global. A shared nonce
  across tenants would leak outcomes between operators and break auditability
  (`EDGE_CASES.md` §5, §13).
- **Reconciliation / GGR.** The provider ledger namespaces accounts by operator
  (`op:acme:ggr`, `op:betco:ggr`) so per-operator **GGR** (gross gaming
  revenue) — the basis for the provider's revenue share / invoicing — is a
  direct sum over that operator's postings.
- **Rate limits, quotas, observability.** Per-tenant, so one operator's traffic
  spike or incident can't starve another (noisy-neighbour isolation).

The engine depends on a small **`OperatorRegistry`** abstraction: given an
`operatorId`, hand back that tenant's config and wallet client, or reject an
unknown operator outright. Operator-specific quirks (wallet API dialect, auth
scheme, currency mapping) live in the **wallet adapter at the edge**; the engine
core stays operator-agnostic — operator-specific behaviour at the edges, clean
interface internally.

---

## 4. The round lifecycle, re-drawn for a provider

The six-step round from `DOMAIN_PRIMER.md` §1 still holds, but steps 2 and 6 —
debit and credit — now cross the seamless boundary, and a tenant-resolution
step is bolted on the front:

0. **Resolve tenant.** Authenticate the operator, look up `TenantConfig` and the
   wallet client. Unknown operator → reject (isolation boundary).
1. **Validate** the bet: game enabled for this tenant, asset allowed, stake
   within this tenant's per-currency limits, target in range.
2. **Publish commitment**, derive `roundId` and `txRef`s, take the per-account
   lock.
3. **`debit`** the stake via the seamless wallet (`txRef = roundId:bet`).
   Declined → round rejected, no outcome generated.
4. **Generate outcome** (RNG, per-session nonce) and **settle** (pure).
5. **Record** the round to the provider reconciliation ledger (per-operator
   GGR postings). *If anything in 4–5 fails after a successful debit →*
   **`rollback`** *the debit and abort.*
6. On a win, **`credit`** the payout (`txRef = roundId:win`). Credit is
   retried, not rolled back — the player *won*; the obligation stands until the
   operator confirms it.

The atomic unit is still "the whole money loop," but the loop now spans two
systems. In-process, the `AsyncMutex` (now **per-account**, not global) plus a
DB transaction guard the provider's own state; correctness *across* the boundary
is carried by `txRef` idempotency and the explicit rollback path, because you
cannot wrap someone else's wallet in your transaction.

---

## 5. Glossary (provider-platform additions)

Grouped, same as `DOMAIN_PRIMER.md` §2 — these are the terms that only appear
once you're the supplier, not the casino.

### Roles & topology

- **RGS (Remote Game Server)** — the provider backend that runs games remotely
  and serves results to operators. This codebase is a (minimal) RGS.
- **Operator** — the licensed casino brand that fronts the games to players and
  **owns the wallet and the player relationship**. Your tenant.
- **Aggregator** — a middle layer that integrates many providers and exposes one
  API to operators. You may sit behind one; the seamless contract is the same.
- **Tenant** — one operator's partitioned slice of the platform.
- **Onboarding / integration** — standing up a new operator: register config,
  exchange callback secrets, point at their wallet endpoint, enable a game set.

### The wallet boundary

- **Seamless / transfer wallet** — operator custodies funds; provider calls
  `debit`/`credit`/`rollback` per money event. §2.
- **`txRef`** — provider-unique transaction reference; the operator's
  idempotency key. The unit of exactly-once across the boundary.
- **Rollback / cancel** — reversing a debit when a round can't complete.
- **Wallet decline** — the operator refusing a debit (insufficient funds, limit
  hit, self-excluded player). A normal outcome, not an error — the round is
  simply rejected.

### Money & accounting (provider side)

- **GGR (Gross Gaming Revenue)** — stakes minus payouts over a period. The
  provider's reconciliation ledger sums GGR **per operator**; revenue share /
  invoicing is computed from it. (NGR — *net* GR — subtracts bonuses and is an
  operator concern.)
- **Reconciliation mirror** — the provider's double-entry ledger is no longer a
  custody record; it's a mirror used to reconcile against the operator's wallet
  and to compute GGR. The operator's wallet is the source of truth for funds.
- **RTP variant** — the same game shipped at different configured RTPs (e.g.
  96% / 94% / 88%); operators pick a variant per jurisdiction. Selected by
  tenant config.

### Isolation

- **Tenant-scoped idempotency** — dedup keyed by `(operatorId, key)`.
- **Per-account lock** — serialise by `(operator, player)` instead of a global
  mutex, so unrelated players don't contend.
- **Seed session** — the `(operator, player, clientSeed)` tuple that owns a
  monotonic nonce sequence.
- **Noisy-neighbour isolation** — per-tenant rate limits/quotas so one operator
  can't degrade another.

---

## 6. What stays exactly the same (and why that's the point)

The whole reason the engine survives this inversion with a small refactor is
that its core invariants are **not** about custody:

- **Determinism & provable fairness** (`rng.js`) — unchanged; if anything,
  *more* important, because now an external operator and their players both
  audit you.
- **Integer/BigInt money & asset tagging** (`asset.js`) — unchanged; amounts
  still cross the wallet boundary as `(asset, BigInt)` pairs.
- **Pure, simulatable settlement** (`game.js`) — unchanged; you still prove RTP
  before shipping a variant.
- **Append-only hash-chained ledger** (`ledger.js`) — unchanged mechanism, new
  *role* (reconciliation/GGR mirror, accounts namespaced by operator).
- **Idempotency discipline** (`round.js`) — unchanged principle, now scoped per
  tenant and extended across the seamless boundary via `txRef`.

The refactor is concentrated where the inversion actually bites: a tenant
boundary in front (`tenant.js`), and a wallet that calls out instead of holding
funds (`seamless.js`).

---

## 7. Common questions about the platform design

- *"You're a game provider, not the casino. Where does the money live?"* →
  On the operator's books. We never custody. We call their seamless wallet
  (`debit`/`credit`/`rollback`) per money event; our ledger is a reconciliation
  and GGR mirror, not a balance of record.
- *"A `debit` succeeds, then your process crashes before settling. What
  happens?"* → On recovery (or via the operator's reconciliation), the
  un-settled bet is **rolled back** by `txRef`; the player gets their stake
  back. Nothing is generated or owed for a round that never produced a result.
- *"A `credit` (payout) call times out. Do you roll back the bet?"* → No. The
  player **won**; rolling back the stake would erase a legitimate win. Credit is
  **idempotent and retried** (same `txRef`) until the operator confirms. Rollback
  is only for un-completable rounds, never for a confirmed win.
- *"Two operators send the same idempotency key. Collision?"* → No. Dedup is
  keyed by `(operatorId, key)`. Tenancy partitions the idempotency store, the
  RNG seed sessions, the secrets, and the reconciliation accounts.
- *"How do you onboard operator #51 without touching the engine?"* → Register a
  `TenantConfig` (enabled games, currencies, limits, jurisdiction) and a wallet
  adapter (endpoint, callback secret, API dialect) in the registry. The engine
  depends on the abstraction; operator quirks stay in the adapter.
- *"How do you compute what to invoice each operator?"* → Per-operator GGR =
  Σ(stakes) − Σ(payouts) over the period, read straight off the namespaced
  postings in the reconciliation ledger; apply the contracted revenue-share rate.
- *"What's different about provable fairness now?"* → The nonce sequence is
  per `(operator, player, clientSeed)` session, not global — otherwise outcomes
  would correlate across tenants. Everything else (commit before bet, reveal
  always, CSPRNG seeds) is unchanged.
