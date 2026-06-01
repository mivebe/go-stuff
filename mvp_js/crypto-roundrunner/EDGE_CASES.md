# Edge cases and restrictions — crypto casino + wallet

A field guide to what bites you in a real-money crypto gaming backend. Grouped
by theme; each item is a concise field note, not a deep dive.
Use this alongside the `roundrunner` JS code — many items have a "where it
shows up" pointer.

---

## 1. Number precision (the JavaScript-specific landmine)

- **Never `Number` for crypto amounts.** `Number.MAX_SAFE_INTEGER` is `2^53 − 1`
  ≈ `9.007 × 10^15`. 1 ETH in wei is `10^18` — well past safe-integer territory.
  Even a routine fee calculation in wei can produce intermediate products that
  blow the safe range. Use `BigInt` for amounts everywhere; the type system
  doesn't enforce it, so a code review discipline does. *(see `src/asset.js`)*
- **You cannot mix `Number` and `BigInt`.** `1n + 1` throws `TypeError`. This
  feels annoying but is a feature — it forces a conversion at every boundary.
- **`Math.random` is not a CSPRNG.** It's seeded predictably and fully
  recoverable from a handful of samples. Use `crypto.randomBytes` /
  `crypto.getRandomValues` for any value an attacker might want to predict
  (seeds, tokens, nonces). *(see `src/rng.js`)*
- **`JSON.stringify` cannot serialise `BigInt`.** It throws by default. Add a
  custom replacer (`(k,v) => typeof v === 'bigint' ? v.toString() : v`) or an
  explicit `toJSON` on your money type. *(Amount.toJSON in `src/asset.js`)*
- **`JSON.parse` won't restore `BigInt`.** A round-trip via JSON downgrades
  numbers — you must hydrate amounts explicitly on the way in.
- **`BigInt` division truncates toward zero, not toward `-∞`.** `-7n / 2n` is
  `-3n` (not `-4n`). For payouts on non-negative stakes truncation equals floor
  and is house-favourable, but the moment you handle fees on negative amounts
  the rounding policy must be re-stated explicitly. *(see `Amount.mulBps`)*
- **`==` vs `===`.** Always `===`. `0n == 0` is true, but `0n === 0` is false —
  loose equality across types is a money-correctness trap.
- **`Date.now()` drifts and can go backwards** (NTP adjustments). Don't use it
  for relative timing or as a tiebreaker; use a monotonic seq number for
  ordering (the ledger does this).
- **Unhandled promise rejection crashes Node 15+** by default. Every async
  money path must catch — wrap engine calls in a top-level handler.

---

## 2. Money correctness

- **Tag every amount with its asset.** Adding BTC to USDT is a bug, not an
  operation. Run the asset check at the type boundary so it fails loud.
  *(Amount enforces this; see the `amount.test.js` mismatch test)*
- **Double-entry must hold PER ASSET, not in aggregate.** A journal that nets
  to zero across assets but not within each is a "money created from thin air"
  bug. The ledger refuses such a journal at write time. *(`ledger.append`)*
- **Define the rounding policy in writing.** Floor (house-favourable),
  round-half-even (regulator-friendly), banker's rounding — pick one, document
  it, apply it uniformly. Inconsistent rounding becomes visible months later as
  unexplained ledger drift.
- **Dust below transferability.** A balance smaller than the on-chain fee to
  move it is locked up. Track per-asset min withdrawal, surface it in UI, and
  decide whether to write off dust below threshold or sweep it during cold
  rebalances.

---

## 3. Idempotency

- **"Just retry" is a bug, not a fix** — only safe when guarded by an
  idempotency key the server stores. Clients may retry without you knowing
  (HTTP timeouts, mobile network flaps, queue redelivery).
- **The store must be durable.** In-memory dedup loses the entire safety
  property on a process restart. In production: a DB unique constraint on the
  key, ideally in the same transaction as the money change.
- **Two retries arriving simultaneously can both miss the cache.** The atomic
  primitive must be "insert OR fetch existing" (a unique constraint, not a
  read-then-write). Otherwise the dedup itself races.
- **Same key, different request = client bug — reject loudly.** Don't silently
  return the stored result; that hides bugs. *(see `ReplayMismatchError`)*
- **Choose a TTL.** Keys live in a hot store; you can't keep them forever. 24
  to 72 hours is typical for retries; longer for high-value flows like
  withdrawals.
- **Multiple layers.** The round is idempotent on one key; the wallet's debit
  and credit are idempotent on derived per-leg refs. A retried round must not
  produce two debits even if it crashed between debit and credit. *(see
  `RoundEngine.execute` deriving `stakeRef` / `payoutRef` from `roundId`)*

---

## 4. Concurrency and atomicity

- **Node is single-threaded but not race-free.** Every `await` is a yield
  point; without explicit serialisation, two concurrent calls can interleave.
  The whole money loop is the unit of atomicity. *(see `AsyncMutex` in
  `src/round.js`)*
- **`balance() then debit()` is TOCTOU.** The check and the act must be one
  operation; the wallet's `debit` does the check and the deduction together
  inside the engine's locked section.
- **In production, the mutex becomes a DB transaction.** A single transaction
  for: debit row update, ledger append, idempotency row insert. Either all
  commit or none do.
- **Per-account locking scales better than a global mutex.** Rounds for
  different players don't need to serialise. A real engine takes a lock keyed
  by `playerAccount` (or by `accountId+asset`).
- **Distributed systems also need fencing tokens** when a lease can be lost
  (e.g. a Redis lock expiring). The DB approach with row locks sidesteps this.

---

## 5. RNG and provable fairness

- **Use a CSPRNG, full stop.** `crypto.randomBytes(32)` for server seeds.
  Never `Math.random`, never your own hash-of-time.
- **One server seed per round (or per short window with rotation).** Reusing a
  server seed across rounds means once revealed, all rounds tied to it are
  predictable.
- **Publish the commitment BEFORE the player commits to a bet.** If the player
  bets first and you choose the seed second, the fairness claim is gone. The
  product UX should make this ordering explicit and visible to users.
- **Reveal the seed regardless of win/loss.** Selective reveal leaks
  information; always-reveal preserves the trust property. Rotate the seed
  after reveal — a revealed seed is dead.
- **Client seed must be entropy the SERVER can't predict.** Otherwise the
  server can pre-grind favourable outcomes. Let the player set it, default to
  a player-side random value, and let them rotate it whenever.
- **Beware modulo bias** when mapping HMAC bytes to a small outcome space.
  `n % ROLL_SPACE` is biased by `ROLL_SPACE / 2^32`; for 10000-outcome dice
  this is `~2 × 10^-6` — negligible, but document it and use rejection
  sampling for stricter games.
- **Nonce must be monotonic and gap-free.** A skipped or reordered nonce
  breaks auditability; an attacker who can replay a nonce against a known seed
  trivially predicts the next outcome.
- **Resist nonce-grinding.** A player who can submit many bets quickly to
  search for a favourable nonce is exploiting the same determinism that gives
  you auditability. Mitigations: server-controlled nonce sequence (which we
  do), seed rotation on reveal, rate limits, fixed clientSeed per session.
- **Don't rely on chain randomness naively.** `block.hash`, `block.timestamp`
  etc. are observable and manipulable by miners/validators. If you go on-chain
  for randomness, use a VRF (Chainlink VRF, drand) with proof.

---

## 6. Ledger and audit

- **Append-only is non-negotiable.** Reversals (chain reorg, wrong credit) are
  new entries, never edits. The ledger is also the bank reconciliation, the
  audit trail, and the source of truth for balances — all at once.
- **Hash chain integrity depends on canonical serialisation.** Two equivalent
  encodings (different key order, BigInt as number vs string) produce
  different hashes. JSON canonicalise with sorted keys and explicit BigInt
  strings. *(see `canonicalize` in `src/ledger.js`)*
- **Time precision is not for ordering.** Use sequence numbers; timestamps
  drift across machines and across log batches.
- **Sign the chain head, not just hash it.** A hash chain detects tampering;
  a signature also proves who wrote each record. Add a per-record HMAC or
  Ed25519 signature for stronger guarantees.
- **Reserves vs liabilities attestation.** A crypto casino's liabilities are
  the sum of player balances in the ledger; the reserves are the on-chain
  holdings. A Merkle-tree proof of liabilities (à la Kraken) lets each user
  verify their balance is included without exposing other users.

---

## 7. Crypto-specific: deposits and confirmations

- **Pending vs confirmed.** A new deposit is observed at zero confirmations;
  it must NOT be bettable until your confirmation threshold is met. *(see
  `notifyDeposit`/`advanceConfirmations`)*
- **N-confirmation thresholds vary by chain and asset.**
  Bitcoin: 2–6 for small, 6+ for large.
  Ethereum (post-merge): 12–32 blocks, or "finalised" (~2 epochs).
  L2s: depend on whether you trust the sequencer (instant) or wait for the L1
  settlement window (often hours/days). Be explicit per chain.
- **Reorgs un-confirm deposits.** A previously-credited deposit can be rolled
  back. The handler must observe the reorg and post a reversal journal —
  never mutate the original credit.
- **Probabilistic vs economic finality.** Bitcoin is probabilistic; even
  6 confirmations is a probability statement. Ethereum PoS finality is
  cryptographic but requires waiting for the finalisation gadget. L2 finality
  depends on the rollup type (optimistic = challenge window; ZK = proof
  verified on L1).
- **Reorg depth heuristics.** Track the deepest reorg you've ever seen on each
  chain and set confirmation thresholds well above it.
- **MEV and front-running** affect on-chain payouts and deposit ordering, not
  the off-chain engine itself, but matter when you batch-process withdrawals.

---

## 8. Crypto-specific: assets, identity, decimals

- **Asset identity = (symbol, chain), not symbol alone.** USDT on Tron, USDT
  on Ethereum, and USDT on BSC are three different assets. Treating them as
  one is a wrong-chain accident waiting to happen. *(see `Asset.key`)*
- **Decimals are per asset and wildly different.** BTC 8, ETH 18, USDC 6,
  XRP 6, SOL 9, DOGE 8. Cross-asset conversion (a swap, a USD-equivalent limit
  check) requires explicit scaling, never "just multiply".
- **ERC-20 contracts declare their own decimals.** Read it from the contract,
  cache it, and re-verify if the contract was upgraded. Don't hardcode.
- **Wrong-chain deposits are often unrecoverable** (USDT-TRC20 sent to an
  ETH-format address, ETH sent to a Bitcoin address). Surface this risk in
  the deposit UI; for chains where recovery is possible, run a recovery flow.
- **Memo/destination tag on XRP, XLM, EOS, etc.** Missing memo = funds in
  a shared hot wallet with no owner. Generate per-user memos and validate on
  the deposit listener.
- **Native vs token on the same chain.** ETH (native) and WETH (ERC-20) are
  different assets in your ledger but related; conversions are explicit user
  actions, not implicit normalisation.

---

## 9. Crypto-specific: withdrawals and addresses

- **Validate per-chain address format** before signing anything. Bech32
  (BTC native segwit), Base58 (BTC legacy), EIP-55 checksum (EVM),
  base32 (Stellar). A typo'd address can be a permanent loss.
- **Screen against sanctions lists and known-bad address sets** (OFAC SDN,
  Chainalysis/TRM screening). Withdrawals to flagged addresses must be
  blocked or held for review.
- **Mixer and high-risk source screening.** Deposits FROM addresses tagged as
  mixers, sanctioned protocols, or known thefts may need to be quarantined or
  rejected per your AML policy.
- **Gas/fee estimation and who pays.** The casino normally pays. Budget the
  fee from the player's withdrawal amount or from a separate operating fund;
  decide and document.
- **EVM nonce management.** Each withdrawal is a transaction with a nonce.
  Stuck transactions, gas underestimation, and nonce gaps freeze the queue.
  Design with replacement-by-fee (bump gas, same nonce) and stuck-tx detection.
- **Replacement-by-fee on Bitcoin.** Players can RBF-replace a deposit
  transaction at low confirmations. Treat 0-conf as "seen", not "received".
- **Withdrawal address poisoning** (clipboard malware substituting addresses).
  Require address book entries, address confirmation step, and rate-limit
  first-time-large withdrawals.
- **Smart-contract vs EOA destinations.** Withdrawing to a contract that
  doesn't accept native ETH burns funds. Detect contract-vs-EOA and warn.
- **Hot/cold wallet split.** The hot wallet has only operational-day funds;
  cold storage is multisig and offline. Only the hot balance is bettable; the
  cold balance is balance-sheet only.

---

## 10. Provably-fair attack vectors

- **Pre-grinding the server seed** (server picks a favourable seed before
  publishing commitment): mitigated by publishing commitment BEFORE
  client seed is set, and by the player providing entropy.
- **Mining the client seed** (player tries many client seeds against a known
  commitment to find a favourable nonce-0 outcome): mitigated by server seed
  rotation after each reveal, and by rate limits on seed changes.
- **Nonce reuse with the same seed pair** is catastrophic — the output
  becomes deterministic and predictable. The engine controls the nonce; the
  player cannot reset it.
- **Commitment hash collision.** SHA-256 has no known practical collision;
  if you ever swap to a weaker hash, the trust property collapses.
- **Reveal timing channels.** If you delay reveals selectively (e.g. only on
  losses), you leak information. Reveal on every round, same path.

---

## 11. Regulatory and responsible gaming

- **Self-exclusion lists** must short-circuit the bet path; an excluded user's
  bet is rejected before reaching the engine, with appropriate UX.
- **Deposit and loss limits** per time window (daily/weekly/monthly), per KYC
  tier. Enforced before the wallet sees the deposit, before the engine sees
  the bet.
- **KYC tiers gate amounts.** Unverified users get a low cap; verified users
  get a higher cap; enhanced-DD users get even higher. Map limits to tiers.
- **Geo-blocking** by IP and KYC country. The intersection (KYC says one
  country, IP says another, conflicting with a restricted list) is a real
  case that needs a rule.
- **Bonus balance is not cash balance.** Bonus funds carry wagering
  requirements; until met, bonus + winnings-from-bonus cannot be withdrawn.
  Model them as separate ledger sub-accounts so the engine can never
  accidentally pay out unwagered bonus.
- **AML thresholds.** Cumulative deposits or withdrawals over thresholds
  trigger SAR (suspicious activity report) review. KYT (know-your-transaction)
  screening on every deposit and withdrawal.
- **Travel Rule.** For VASP-to-VASP transfers above thresholds (often
  ~$1000 / €1000), originator and beneficiary information must be exchanged.
  Affects withdrawals to known exchange addresses.
- **License jurisdiction shapes everything.** Curacao, Malta, Isle of Man,
  Anjouan each have different operational requirements; what's required is
  partly a function of where you're licensed and where you serve users.

---

## 12. Operations and observability

- **Per-round metrics.** p50/p95/p99 latency, debit/credit error rates,
  ledger-append failures, idempotency-replay hit rate, ReplayMismatch rate
  (client bugs), insufficient-funds rate.
- **Realised vs configured RTP drift.** Over a rolling window, realised RTP
  should hover near configured. Sustained drift outside variance bounds is
  either a config bug, a game bug, or an attack — alert.
- **Reserve attestation job.** Periodically reconcile on-chain reserves (per
  asset) against ledger liabilities (sum of player balances). A diff outside
  tolerance is a four-alarm incident.
- **Hash-chain verification job** runs on schedule; a failure is a security
  incident, not a metric blip.
- **Disaster recovery from the ledger.** If the wallet cache is corrupted,
  rebuild balances by replaying the ledger. This works only if the ledger
  has been the source of truth all along.
- **Player exclusion sweeps.** Self-exclusion list updates apply
  retroactively to live sessions, not just future logins.

---

## 13. Multi-tenancy and operator isolation (game-provider platform)

The shift from "we are the casino" to "we are a game provider serving many
operators" is in `PLATFORM_PRIMER.md`. These are the things that bite once the
tenant is the *operator*, not the player.

- **The tenant is the operator, not the player.** `player:alice` under operator
  `acme` and `player:alice` under `betco` are different people. Every account,
  key, and metric must carry the operator dimension or two operators silently
  share state. *(see `src/tenant.js`)*
- **Idempotency keys are tenant-scoped.** Dedup on `(operatorId, key)`, never the
  bare key. Two operators independently sending `"k1"` is normal; collapsing
  them returns operator A's result to operator B — a cross-tenant data leak and
  a money bug at once. *(see the scoped key in `RoundEngine.execute`)*
- **RNG seed sessions are per (operator, player, clientSeed).** A global nonce
  counter shared across tenants leaks outcome ordering between operators and
  breaks per-operator auditability. Namespace the seed session. *(see
  `#nonces` keying in `src/round.js`)*
- **Callback secrets are per operator.** Each operator's seamless traffic is
  signed/verified with *its own* HMAC key. One operator's key validating
  another's requests is a critical auth break. Never a shared platform secret.
- **Reconciliation accounts are namespaced by operator.** `op:acme:ggr` and
  `op:betco:ggr` are distinct; per-operator GGR (the revenue-share basis) is a
  sum over that operator's postings. Mixed accounts make invoicing unprovable.
- **Unknown operator → reject before any work.** An unrecognised `operatorId`
  must fail closed at the front door, before bet validation, RNG, or any wallet
  call. *(see `UnknownOperatorError`)*
- **Per-tenant limits and game enablement.** Bet min/max, enabled games, and
  allowed assets are per operator and per jurisdiction (RTP variant included).
  The engine enforces the *tenant's* limits, not a global default.
- **Noisy-neighbour isolation.** Per-tenant rate limits and quotas so one
  operator's spike or incident can't starve another. Locking is **per
  (operator, player)**, not a global mutex — unrelated players never contend.
- **Per-tenant observability.** Latency, decline rate, rollback rate, GGR drift —
  all sliced by operator. A platform-wide average hides the one operator whose
  wallet is timing out.
- **Config/secret rotation is live.** Onboarding operator #51, rotating a
  callback secret, or disabling a game must not require a redeploy or touch the
  engine core — it's registry data.

---

## 14. Seamless wallet integration (the boundary that can fail)

In the seamless model the operator custodies funds; the provider calls their
wallet API (`debit`/`credit`/`rollback`) on every money event. The boundary is
a network call to *another company's* system, so every distributed-systems
hazard applies — with money on the line. *(see `src/seamless.js`)*

- **`txRef` is the unit of exactly-once across the boundary.** Provider-unique,
  globally stable per leg (`roundId:bet`, `roundId:win`). The operator dedupes
  on it. A retried `debit` with the same ref must be a no-op returning the
  original result — never a second charge. *(idempotency, §3, stretched across
  an inter-company boundary where retries are guaranteed)*
- **Debit decline is a normal outcome, not an error.** Insufficient funds, a
  hit limit, or a self-excluded player → the operator refuses the debit and the
  round is simply rejected. No outcome is generated. Don't treat it as a 500.
- **Never generate an outcome before the debit confirms.** Same rule as the
  custodial engine: secure the stake first. The difference is the debit is now
  a remote call that can time out.
- **Rollback is for un-completable rounds, NOT for losses or for failed
  payouts.** If a `debit` succeeded but settlement/ledger-commit fails (or the
  process crashes before producing a result), `rollback` the debit by its
  `betTxRef`. A *loss* is a completed round — never rolled back.
- **A failed `credit` is retried, never converted to a rollback.** The player
  won; rolling back the stake would erase a legitimate win. Credit is
  idempotent on its `txRef`; retry it (in-band, then out-of-band) until the
  operator confirms. The obligation stands. *(this asymmetry — rollback debits,
  retry credits — is the single most important rule of the boundary)*
- **Timeout ≠ failure.** A timed-out `debit`/`credit` may have *succeeded* on
  the operator side with the response lost. Because the call is idempotent on
  `txRef`, the safe move is to **retry the same ref**: it either confirms the
  prior success or applies once. Never assume a timeout means "didn't happen."
- **Reconcile, don't trust.** Run a periodic job that compares the provider's
  reconciliation ledger against the operator's wallet statement per `txRef`.
  Orphaned debits (debited, never settled, never rolled back) and missing
  credits are the residue of boundary failures and must be swept.
- **The operator's wallet is the source of truth for funds.** The provider's
  ledger is a mirror for reconciliation and GGR. On disagreement, the operator's
  custody wins; the provider investigates its own gap.
- **Order and atomicity can't span both systems.** You cannot wrap the
  operator's wallet in your DB transaction. Correctness comes from `txRef`
  idempotency + the rollback path, not from a distributed transaction. Avoid
  two-phase commit fantasies; design for retry and reconciliation.
- **Currency mapping lives in the adapter.** The operator may denominate in
  fiat or a chain/asset that needs mapping to your `(symbol, chain)` asset
  identity (§8). Map at the edge; keep the engine on tagged `Amount`s.
- **Per-leg idempotency, not per-round.** A retried round must not produce two
  debits even if it crashed *between* debit and credit. Derive both legs'
  `txRef`s from the `roundId` so each leg dedupes independently. *(see
  `stakeTxRef`/`payoutTxRef` derivation in `src/round.js`)*
