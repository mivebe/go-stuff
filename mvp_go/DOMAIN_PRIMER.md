# Real-Money Gaming Backend - Domain Primer

A primer on the domain behind a real-money game round engine. The goal is that
none of the vocabulary feels foreign, and that you can reason about *why* each
constraint exists, not just name it.

The companion code project (`roundrunner`) implements most of what's below, so
read this alongside it: concept here, code there.

---

## 1. The mental model: what is a "round"?

A **round** is one complete unit of play with a deterministic money outcome:
the player commits a **stake**, the system produces an **outcome**, and money
moves accordingly. Everything in this domain is organised around making the
round **correct, auditable, and replayable**.

The lifecycle, from outcome generation through to ledger commit, is roughly:

1. **Validate** the bet against the game config (limits, currency, selection).
2. **Reserve / debit** the stake from the player's wallet.
3. **Generate the outcome** (the RNG step).
4. **Settle**: compute win/loss and payout from outcome + game rules.
5. **Commit to the ledger**: record the money movement, append-only.
6. **Credit** any winnings back to the wallet.

The hard part isn't any single step - it's making steps 2–6 behave as **one
atomic, idempotent, auditable event**, even when the network fails halfway
through. That's the whole job.

---

## 2. Glossary, grouped by theme

### Money correctness

- **Minor units** - the smallest indivisible unit of a currency (cents for USD,
  satoshis for BTC). Money is stored as an **integer count of minor units**,
  never a float. `0.1 + 0.2 != 0.3` in floating point; in a ledger that's a
  financial incident. `int64` of minor units is exact.
- **Double-entry** - every money movement is recorded as balanced postings that
  sum to zero. Money is never created or destroyed, only moved between
  accounts. If a journal doesn't balance, it's a bug, and the ledger should
  refuse it at write time.
- **Idempotency key** - a caller-supplied unique ID for an operation. If the
  same key arrives twice, the system returns the original result instead of
  doing the work again. This is *the* reason "just retry" is safe - without it,
  a retried bet is a double charge. "Just retry" is a bug, not a fix, unless it's
  guarded: retry is only safe *because of* idempotency, not on its own.
- **Reconciliation** - proving two independent records of money agree (e.g. the
  cached wallet balance vs. the sum of ledger postings). They must always match.

### RNG, fairness, and auditability

- **RNG (Random Number Generation)** - how outcomes are produced. In real-money
  gaming it must be unpredictable to the player *and* defensible to a regulator.
- **Determinism** - given the same inputs (seeds, nonce), outcome generation
  produces the same result every time. This sounds odd for "random," but it's
  what makes a round **replayable and auditable**: an auditor can recompute the
  exact outcome from recorded inputs.
- **Commit-reveal / provably fair** - the server publishes a hash of a secret
  **server seed** *before* the round (the **commitment**), the player
  contributes a **client seed**, the outcome is derived deterministically from
  both, and the server **reveals** the seed afterwards so anyone can verify the
  result was fixed in advance and not tampered with.
- **Nonce** - a per-round counter mixed into outcome generation so the same
  seed pair produces different outcomes each round.
- **Signed audit logs** - audit records cryptographically signed by the writer,
  so their authenticity (not just integrity) is provable. A hash chain proves
  *tamper-evidence*; a signature proves *who wrote it*.
- **Tamper-evident / append-only** - records are only added, never edited or
  deleted, and each is chained to the previous via a hash, so altering history
  is detectable.

### Game economics

- **RTP (Return To Player)** - the long-run fraction of stakes returned as
  winnings, e.g. 98%. The complement is the **house edge** (2%). RTP is a
  property of the game *config*, and you must be able to prove a game hits its
  configured RTP before it ships ("simulation and verification tooling that
  proves game logic").
- **House edge** - `1 - RTP`. The structural margin the operator keeps.
- **Bet limits / currency mapping** - per-game, per-operator, per-currency
  min/max stakes and how amounts map across currencies. Part of the "game
  configuration system."
- **Payout multiplier** - how much a winning stake returns (e.g. 1.96x). Even
  this is computed in integers with an explicit rounding policy.

### Transport and services

- **gRPC** - a binary, contract-first RPC protocol over HTTP/2, used for
  **service-to-service** (internal) communication. Strongly typed via protobuf.
- **REST** - JSON over HTTP, used for **client-facing** request/response
  surfaces.
- **WebSocket** - a persistent, bidirectional connection used for **real-time**
  client surfaces (live game state, streaming results) where polling REST would
  be too slow.
- **Contract-first** - the API contract (protobuf / OpenAPI) is the source of
  truth, written before implementation; **contract tests** verify both sides
  conform, and gate merges.

### Multi-tenancy and operators

- **Operator / operator partner** - a brand or platform that surfaces your
  games to end players. They often own the **player wallet**; your engine
  integrates with theirs.
- **Wallet integration** - the boundary where your engine debits stakes and
  credits winnings against the operator's funds, over an API. Keeping
  "operator-specific behaviour at the edges" means a clean interface internally,
  with per-operator quirks isolated to adapter code.
- **Multi-tenant / white-label** - one platform serving many operators, with
  data, config, and behaviour partitioned per tenant ("tenant-aware from day
  one"). **Operator onboarding** is the process of integrating a new one.

### Reliability and operations

- **Observability** - metrics, structured logs, and traces that let you see
  what the system is doing in production (especially money flows and latencies).
- **Idempotency + at-least-once delivery** - distributed systems usually
  guarantee a message arrives *at least* once, so consumers must be idempotent
  to achieve *effectively* once.
- **AWS ECS** - Elastic Container Service; runs your containers. The platform
  team owns it; you're expected to understand what's underneath without
  operating it.
- **Low-latency request path** - the hot path a player's action travels;
  shaving milliseconds and avoiding blocking work matters here.

---

## 3. The non-negotiables, and *why* (rehearse these)

These four come up over and over; be able to defend each in one breath.

- **Determinism** - so any round can be recomputed and audited from its
  recorded inputs. No "we can't reproduce what the player saw."
- **Idempotency** - so retries (which *will* happen) don't move money twice. The
  network is unreliable by definition; correctness can't depend on it being
  reliable.
- **Auditability** - so a regulator, an operator, or an incident responder can
  reconstruct exactly what happened to every cent, in order, after the fact.
- **Integer money** - so arithmetic is exact. Floats silently lose pennies, and
  in aggregate that's both a compliance failure and real money.

What matters is being able to say *why* each one is non-negotiable, not just
that it is — the reasoning is the point.

---

## 4. A roadmap for going deeper

Starting from HTTP, request/response shapes, and how a client consumes an API,
these are the backend areas to work through, in rough order:

1. **Go fundamentals for backend** - goroutines, channels, `context.Context`,
   interfaces, error wrapping (`%w`), `sync.Mutex`. Build and read the
   `roundrunner` code until none of it is mysterious.
2. **The money loop** - internalise the round lifecycle by extending the MVP
   (add a new game, add a balance endpoint, break the ledger on purpose and
   watch `Verify` catch it).
3. **Idempotency & distributed correctness** - read about idempotency keys,
   exactly-once vs at-least-once, and why DB transactions + unique constraints
   are the usual real implementation. Replace the in-memory idempotency map with
   a sketch of the SQL version.
4. **gRPC + protobuf** - define the round service as a `.proto`, generate Go,
   and put a gRPC surface in front of the same engine. This directly mirrors the
   "gRPC internally, REST/WS at the edge" architecture.
5. **Provable fairness** - make sure you can explain commit-reveal end to end
   and verify a round by hand. It's a strong property to be able to demonstrate.
6. **AWS ECS basics** - containers, task definitions, services, load balancing,
   health checks. Enough to discuss deploys intelligently, not to run a cluster.
7. **Observability** - what you'd emit as metrics/logs/traces for a money
   system (per-round latency, debit/credit failures, ledger append errors,
   idempotency-hit rate).

---

## 5. Common questions about the design (and the shape of a good answer)

- *"Walk me through a round."* → The six-step lifecycle in §1, calling out
  where atomicity and idempotency live.
- *"A bet request times out and the client retries. What happens?"* → The
  idempotency key means the retry either finds the completed round and returns
  it, or (if the first attempt never committed) executes fresh - exactly once
  either way. Never two debits.
- *"Why integer money?"* → Float can't represent decimal cents exactly; errors
  accumulate; ledgers must be exact. Minor units in `int64`, explicit rounding.
- *"How do you prove a game's RTP before launch?"* → Pure, deterministic
  settlement logic + a simulation that runs millions of rounds and checks
  realised RTP against the configured target (the MVP does 100k and lands on
  ~98%).
- *"How is the ledger tamper-evident?"* → Append-only + hash chain (each record
  hashes the previous one); altering history breaks every subsequent hash.
  Add signing to also prove authorship.
- *"How would you onboard a new operator?"* → Operator-specific behaviour behind
  a wallet/integration interface; the engine depends on the abstraction, the
  adapter holds the quirks; per-tenant config drives limits and currency.
- *"What would you put on a dashboard for this?"* → Per-round latency
  percentiles, debit/credit error rates, ledger append failures, idempotency
  replay rate, realised vs configured RTP drift, balance reconciliation alerts.
