# Edge cases and restrictions — crypto game provider + wallet (Go)

A field guide to what bites you in a real-money crypto gaming backend. Grouped by
theme; each item is a concise field note, not a deep dive. Use it alongside the
Go code — many items have a "where it shows up" pointer. This is the Go sibling
of `../../mvp_js/crypto-roundrunner/EDGE_CASES.md`; §1 is the part that differs
most by language.

---

## 1. Number precision (the Go-specific landmine)

- **Never `float64` for crypto amounts.** Floats can't represent decimal cents
  exactly; errors accumulate and a ledger that's "off by a satoshi" is a
  compliance incident. Money is an integer count of base units, always.
- **`int64` is not enough for crypto.** `int64` maxes at ~9.2 × 10^18. 1 ETH in
  wei is 10^18 (fits), but **ten ETH is 10^19 and overflows**, and intermediate
  products in fee math overflow far sooner — silently, with wraparound. Use
  `math/big.Int` for amounts everywhere. *(see `internal/asset`)*
- **Tag every amount with its asset.** `asset.Amount` is an `(Asset, *big.Int)`
  pair; `Add`/`Sub`/`Lt` panic on an asset mismatch. Adding BTC to USDT is a bug,
  not an operation — fail loud at the boundary.
- **`big.Int` division truncates toward zero.** `(-7).Quo(2) == -3`, not `-4`.
  For payouts on non-negative stakes truncation equals floor and is
  house-favourable; the moment you handle fees on negative amounts the rounding
  policy must be re-stated. `asset.MulBps` panics on a negative amount rather than
  paper over it. *(see `Amount.MulBps`)*
- **`big.Int` is a pointer type — aliasing bites.** Two variables can share one
  backing array; an in-place op on one mutates the other. `asset.Amount` defends
  against this by copying on construction and returning fresh values from every
  operation; never hand a caller your internal `*big.Int`. *(see `Amount.Value`)*
- **Use `crypto/rand`, never `math/rand`, for anything an attacker might predict.**
  `math/rand` is a deterministic PRNG recoverable from a few samples. Seeds,
  tokens, nonces all come from `crypto/rand`. *(see `internal/rng`)*
- **JSON can't carry a `big.Int` as a number safely.** Encode amounts as a
  decimal **string** and hydrate them explicitly on the way in; a JSON number
  round-trip through a `float64` decoder loses precision. *(see
  `Amount.MarshalJSON` → `{"asset":..,"v":"<decimal>"}`)*
- **Wall-clock time is not for ordering.** `time.Now()` can jump backwards (NTP).
  Use a monotonic sequence number for ordering (the ledger does — `Journal.Seq`),
  not timestamps.

---

## 2. Money correctness

- **Tag every amount with its asset.** Adding BTC to USDT is a bug, not an
  operation. Run the asset check at the type boundary so it fails loud.
- **Double-entry must hold PER ASSET, not in aggregate.** A journal that nets to
  zero across assets but not within each is a "money created from thin air" bug.
  The ledger refuses such a journal at write time. *(see `ledger.Ledger.Append`)*
- **Define the rounding policy in writing.** Floor (house-favourable),
  round-half-even (regulator-friendly), banker's rounding — pick one, document it,
  apply it uniformly. Inconsistent rounding surfaces months later as unexplained
  ledger drift.
- **Dust below transferability.** A balance smaller than the on-chain fee to move
  it is locked up. Track per-asset min withdrawal, surface it, and decide whether
  to write off dust below threshold or sweep it during cold rebalances.

---

## 3. Idempotency

- **"Just retry" is a bug, not a fix** — only safe when guarded by an idempotency
  key the server stores. Clients retry without you knowing (HTTP timeouts, mobile
  flaps, queue redelivery).
- **Tenant-scoped.** The dedup key is `(operatorId, idempotencyKey)`, never the
  bare key — two operators can independently send `"k1"`. *(see `round.Engine`
  `scopedKey`)*
- **Across the wallet boundary it's `TxRef`.** The round is idempotent on one
  scoped key; the wallet's debit/credit/rollback are each idempotent on a derived
  per-leg `TxRef` (`roundID:bet`, `roundID:win`). A retried round must not produce
  two debits even if it crashed between debit and credit. *(see `seamless`)*
- **The store must be durable.** In-memory dedup loses the safety property on a
  restart. In production: a DB unique constraint on the key, ideally in the same
  transaction as the money change.
- **Two retries arriving simultaneously can both miss the cache.** The atomic
  primitive must be "insert OR fetch existing" (a unique constraint), not
  read-then-write, or the dedup itself races.
- **Same key, different request = client bug — reject loudly.** Don't silently
  return the stored result; that hides bugs. *(see `round.ErrReplayMismatch`)*
- **Choose a TTL.** 24–72 hours is typical for retries; longer for high-value
  flows like withdrawals.

---

## 4. Concurrency and atomicity

- **Goroutines are truly concurrent.** Unlike the JS sibling's single-threaded
  `await` interleaving, Go needs real locks. The whole money loop is the unit of
  atomicity; it runs under a per-`(operator, player)` `sync.Mutex`. *(see
  `round.Engine.lockFor`)*
- **`balance() then debit()` is TOCTOU.** The check and the act must be one
  operation; the wallet's `Debit` does the check and the deduction together under
  its own lock, inside the engine's per-account section.
- **In production, the lock becomes a DB transaction.** A single transaction for
  the provider's own state (ledger append, idempotency row); the operator's
  wallet is reached over the boundary and reconciled by `TxRef`, not enrolled in
  your transaction.
- **Per-account locking scales better than a global mutex.** Rounds for different
  players don't serialise. The engine keys its lock by `(operator, player)`.
- **Guard shared maps.** The idempotency store, nonce sessions, and lock table are
  shared across goroutines; each access takes the engine's `sync.Mutex` briefly.
  Go's race detector (`go test -race`) is the tool that proves it.

---

## 5. RNG and provable fairness

- **Use a CSPRNG, full stop.** `crypto/rand` for server seeds. Never `math/rand`.
- **One server seed per round (or per short window with rotation).** Reusing a
  server seed means once revealed, all rounds tied to it are predictable.
- **Publish the commitment BEFORE the player commits to a bet.** If the player
  bets first and you choose the seed second, the fairness claim is gone.
- **Reveal the seed regardless of win/loss.** Selective reveal leaks information;
  always-reveal preserves the trust property. Rotate the seed after reveal.
- **Client seed must be entropy the SERVER can't predict.** Otherwise the server
  can pre-grind favourable outcomes. Let the player set and rotate it.
- **Beware modulo bias** when mapping HMAC bytes to a small outcome space.
  `n % RollSpace` is biased by `RollSpace / 2^32`; for a 10000-outcome dice this is
  `~2 × 10^-6` — negligible, but document it and use rejection sampling for
  stricter games. *(see `rng.Generate`)*
- **Nonce must be monotonic and gap-free, and per seed session.** The engine
  controls the nonce per `(operator, player, clientSeed)`; the player cannot reset
  it. A skipped/reordered/replayed nonce against a known seed breaks auditability
  and predictability.
- **Resist nonce-grinding.** A player submitting many bets quickly to search for a
  favourable nonce exploits the same determinism that gives you auditability.
  Mitigations: server-controlled nonce, seed rotation on reveal, rate limits.
- **Don't rely on chain randomness naively.** `block.hash`/`block.timestamp` are
  observable and manipulable. If you go on-chain, use a VRF (Chainlink VRF, drand)
  with proof.

---

## 6. Ledger and audit

- **Append-only is non-negotiable.** Reversals (reorg, wrong credit) are new
  entries, never edits. The ledger is the bank reconciliation, the audit trail,
  and the basis for GGR all at once.
- **Hash-chain integrity depends on canonical serialisation.** Two equivalent
  encodings (different key order, BigInt as number vs string) produce different
  hashes. Go's `encoding/json` emits struct fields in declaration order and
  `asset.Amount` marshals to a stable `{asset, v-string}` shape. *(see
  `ledger.hashRecord`)*
- **Time precision is not for ordering.** Use sequence numbers (`Journal.Seq`);
  timestamps drift across machines and log batches.
- **Sign the chain head, not just hash it.** A hash chain detects tampering; a
  signature also proves who wrote each record. Add a per-record HMAC or Ed25519
  signature for stronger guarantees.
- **Reserves vs liabilities attestation.** In a custodial model, liabilities are
  the sum of player balances; reserves are on-chain holdings. A Merkle-tree proof
  of liabilities lets each user verify inclusion without exposing others. In the
  provider model the operator owns this; your ledger is a GGR mirror.

---

## 7. Crypto-specific: deposits and confirmations

- **Pending vs confirmed.** A new deposit is observed at zero confirmations; it
  must NOT be bettable until your threshold is met. *(see
  `cryptowallet.NotifyDeposit` / `AdvanceConfirmations`)*
- **N-confirmation thresholds vary by chain and asset.** Bitcoin 2–6 (more for
  large); Ethereum PoS 12–32 blocks or "finalised"; L2s depend on whether you
  trust the sequencer or wait for L1 settlement. Be explicit per chain.
- **Reorgs un-confirm deposits.** A previously-credited deposit can roll back. The
  handler posts a reversal journal — never mutates the original credit. (The MVP
  `cryptowallet` does not model reorgs; this is the gap to close.)
- **Probabilistic vs economic finality.** Bitcoin is probabilistic; Ethereum PoS
  finality is cryptographic but requires the finalisation gadget; L2 finality
  depends on the rollup type.
- **Reorg-depth heuristics.** Track the deepest reorg seen per chain and set
  thresholds well above it.

---

## 8. Crypto-specific: assets, identity, decimals

- **Asset identity = (symbol, chain), not symbol alone.** USDT on Tron, Ethereum,
  and BSC are three different assets. Treating them as one is a wrong-chain
  accident waiting to happen. *(see `asset.Asset.Key`)*
- **Decimals are per asset and wildly different.** BTC 8, ETH 18, USDC 6, XRP 6,
  SOL 9, DOGE 8. Cross-asset conversion requires explicit scaling, never "just
  multiply".
- **ERC-20 contracts declare their own decimals.** Read it from the contract,
  cache it, re-verify on upgrade. Don't hardcode.
- **Wrong-chain deposits are often unrecoverable** (USDT-TRC20 to an ETH address).
  Surface the risk in the deposit UI; run a recovery flow where possible.
- **Memo/destination tag on XRP, XLM, EOS, etc.** Missing memo = funds in a shared
  hot wallet with no owner. Generate per-user memos and validate on the listener.
- **Native vs token on the same chain.** ETH and WETH are different assets in your
  ledger; conversions are explicit user actions, not implicit normalisation.

---

## 9. Crypto-specific: withdrawals and addresses

- **Validate per-chain address format** before signing anything (Bech32, Base58,
  EIP-55, base32). A typo'd address can be permanent loss.
- **Screen against sanctions / known-bad sets** (OFAC SDN, Chainalysis/TRM).
  Withdrawals to flagged addresses are blocked or held.
- **Mixer / high-risk source screening.** Deposits from tagged addresses may need
  quarantine or rejection per AML policy.
- **Gas/fee estimation and who pays.** Usually the casino. Budget it from the
  withdrawal or a separate operating fund; decide and document.
- **EVM nonce management.** Each withdrawal is a transaction with a nonce; stuck
  txs, gas underestimation, and nonce gaps freeze the queue. Design for
  replacement-by-fee and stuck-tx detection.
- **Replacement-by-fee on Bitcoin.** Players can RBF-replace a deposit at low
  confirmations. Treat 0-conf as "seen", not "received".
- **Hot/cold wallet split.** The hot wallet holds operational-day funds; cold is
  multisig and offline. Only the hot balance is bettable.

In the **provider topology these are the operator's problem** — you never custody
funds or sign withdrawals. They're here because the custodial `cryptowallet`
captures the lifecycle, and because you must reason about them when reconciling.

---

## 10. Provably-fair attack vectors

- **Pre-grinding the server seed** — mitigated by publishing the commitment BEFORE
  the client seed is set, and by player entropy.
- **Mining the client seed** — mitigated by server-seed rotation after each
  reveal and rate limits on seed changes.
- **Nonce reuse with the same seed pair** is catastrophic — output becomes
  predictable. The engine controls the nonce; the player can't reset it.
- **Commitment hash collision.** SHA-256 has no practical collision; a weaker hash
  collapses the trust property.
- **Reveal timing channels.** Delaying reveals selectively (e.g. only on losses)
  leaks information. Reveal on every round, same path.

---

## 11. Multi-tenancy and the seamless boundary

- **Tenant = operator, not player.** A player exists only within an operator's
  namespace; same name under two operators = two people. *(see `op:<id>:player:<p>`
  ledger accounts)*
- **Fail closed on unknown operators.** Resolving an unregistered operator throws
  before any work. *(see `tenant.Registry.Resolve` → `*tenant.UnknownOperatorError`)*
- **Per-operator secrets.** One operator's callback secret must never validate
  another's seamless traffic. *(see `tenant.Config.CallbackSecret`)*
- **A decline is normal; a timeout is not.** A `Debit` decline
  (`*seamless.DeclinedError`) rejects the round. A transient failure
  (`*seamless.UnavailableError`) on the credit leg means **retry the same TxRef**
  — the player won; never roll a confirmed win back. *(see
  `round.SettlementPending`)*
- **Rollback only for un-completable rounds.** A failed settlement before a result
  rolls the debit back. A failed credit after a win does NOT — the payout is owed
  and swept out of band via the same `TxRef`.
- **Orphaned debits get reconciled.** If a rollback itself fails (boundary down),
  the orphaned debit is swept by reconciliation; don't mask the original error.

---

## 12. Regulatory and responsible gaming (the operator's surface)

In the provider topology most of this is the **operator's** responsibility,
enforced before the bet reaches your engine. You must know it exists and where it
plugs in.

- **Self-exclusion lists** short-circuit the bet path before the engine.
- **Deposit and loss limits** per window, per KYC tier, enforced operator-side.
- **KYC tiers gate amounts.** Map limits to tiers.
- **Geo-blocking** by IP and KYC country; the conflicting-signal case needs a rule.
- **Bonus balance is not cash balance.** Model bonus funds as separate ledger
  sub-accounts with wagering requirements so unwagered bonus can never be paid out.
- **AML thresholds + KYT** trigger SAR review; screen every deposit/withdrawal.
- **Travel Rule** for VASP-to-VASP transfers above thresholds.
- **License jurisdiction shapes everything** (Curacao, Malta, Isle of Man,
  Anjouan); `tenant.Config.Jurisdiction` is where a real system would branch.

---

## 13. Operations and observability

- **Per-round metrics.** p50/p95/p99 latency, debit/credit error rates,
  ledger-append failures, idempotency-replay hit rate, ReplayMismatch rate (client
  bugs), decline rate, payout-pending rate.
- **Realised vs configured RTP drift.** Over a rolling window, realised RTP should
  hover near configured; sustained drift is a config bug, a game bug, or an
  attack — alert. *(the simulation shows ~98% over 50k rounds)*
- **Per-operator GGR.** Sum over `op:<id>:ggr` per asset is the revenue-share /
  invoicing basis; reconcile it against the operator's own figures.
- **Hash-chain verification job** runs on schedule; a failure is a security
  incident, not a metric blip. *(see `ledger.Verify`)*
- **Disaster recovery from the ledger.** If a cache is corrupted, rebuild balances
  by replaying the ledger — works only if the ledger has been the source of truth
  all along.
- **`go test -race` in CI.** The whole money loop is concurrent; the race detector
  is the cheapest proof that the locking holds.
