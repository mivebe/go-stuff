// round.js
//
// Tenant-aware, idempotent gameplay runtime for a game-provider platform (an
// RGS). One engine serves MANY operators; each round is fully described by
// Request -> Result. See PLATFORM_PRIMER.md §4.
//
// What changed vs the single-tenant custodial engine:
//
//   - Tenancy. Every request names an operator; the engine resolves the tenant
//     (config + seamless wallet) via the OperatorRegistry, or rejects an
//     unknown operator. Idempotency, RNG seed sessions, locks, and ledger
//     accounts are all namespaced by operator.
//   - Seamless wallet. The engine no longer holds funds. It calls the
//     operator's wallet (debit / credit / rollback) over a boundary. The
//     provider ledger becomes a per-operator GGR reconciliation mirror, not a
//     custody record.
//   - Per-(operator,player) locking instead of one global mutex, so unrelated
//     players don't serialise.
//   - Per-(operator,player,clientSeed) nonce sessions, so provable-fairness
//     outcomes never correlate across tenants.
//
// Atomicity still wraps "the whole money loop," but the loop now spans two
// systems. In-process state is guarded by the per-account lock (a DB
// transaction in production); correctness ACROSS the boundary is carried by
// txRef idempotency and the explicit rollback path — you can't wrap someone
// else's wallet in your transaction.

import { Amount } from "./asset.js";
import { newServerSeed, generate } from "./rng.js";
import { WalletDeclinedError } from "./seamless.js";

export class ReplayMismatchError extends Error {
  constructor() {
    super("idempotency key reused with a different request");
    this.name = "ReplayMismatchError";
  }
}

export class GameNotEnabledError extends Error {
  constructor(operatorId, gameName) {
    super(`game ${gameName} not enabled for operator ${operatorId}`);
    this.name = "GameNotEnabledError";
  }
}

export class AssetNotAllowedError extends Error {
  constructor(operatorId, assetKey) {
    super(`asset ${assetKey} not allowed for operator ${operatorId}`);
    this.name = "AssetNotAllowedError";
  }
}

// Raised when a player won but the operator's wallet could not be credited
// after retries. The round is recorded; the payout is owed and must be
// re-credited out of band (same txRef — idempotent). NEVER rolled back: the
// player won. (EDGE_CASES.md §14)
export class PayoutPendingError extends Error {
  constructor(roundId, payoutTxRef) {
    super(`payout pending for ${roundId} (txRef ${payoutTxRef})`);
    this.name = "PayoutPendingError";
    this.roundId = roundId;
    this.payoutTxRef = payoutTxRef;
  }
}

export class RoundEngine {
  #games; // Map(gameName -> GameConfig)
  #registry; // OperatorRegistry
  #ledger; // provider-side reconciliation / GGR mirror
  #locks = new Map(); // lockKey -> AsyncMutex   (per operator|player)
  #nonces = new Map(); // seedSessionKey -> bigint
  #seq = 0n; // monotonic round sequence (round-id uniqueness, not money order)
  #seen = new Map(); // scopedKey -> Result   (tenant-scoped idempotency)
  #fingerprints = new Map(); // scopedKey -> request fingerprint
  #creditRetries;

  /** @param {{
   *    games: Map<string, import("./game.js").GameConfig>,
   *    registry: import("./tenant.js").OperatorRegistry,
   *    ledger: import("./ledger.js").Ledger,
   *    creditRetries?: number,
   *  }} deps */
  constructor({ games, registry, ledger, creditRetries = 1 }) {
    this.#games = games;
    this.#registry = registry;
    this.#ledger = ledger;
    this.#creditRetries = creditRetries;
  }

  /** @param {{
   *    operatorId: string,
   *    playerId: string,
   *    gameName: string,
   *    clientSeed: string,
   *    idempotencyKey: string,
   *    bet: { stake: Amount, target: number },
   *  }} req */
  async execute(req) {
    if (!req.idempotencyKey) throw new Error("idempotency key required");

    // 0. Resolve tenant. Unknown operator fails closed BEFORE any work.
    const { config: tenant, wallet } = this.#registry.resolve(req.operatorId);

    // 1. Validate against THIS tenant's enablement and limits, then the game.
    const game = this.#games.get(req.gameName);
    if (!game || !tenant.allowsGame(req.gameName)) {
      throw new GameNotEnabledError(req.operatorId, req.gameName);
    }
    if (!tenant.allowsAsset(req.bet.stake.asset)) {
      throw new AssetNotAllowedError(req.operatorId, req.bet.stake.asset.key);
    }
    game.validateBet(req.bet);
    this.#enforceTenantLimits(tenant, req.bet);

    const lockKey = `${req.operatorId}|${req.playerId}`;
    const scopedKey = `${req.operatorId}|${req.idempotencyKey}`;
    const seedKey = `${req.operatorId}|${req.playerId}|${req.clientSeed}`;

    const mu = this.#lockFor(lockKey);
    await mu.acquire();
    try {
      // Tenant-scoped idempotency: (operatorId, key), never the bare key.
      const fp = fingerprint(req);
      if (this.#seen.has(scopedKey)) {
        if (this.#fingerprints.get(scopedKey) !== fp) throw new ReplayMismatchError();
        return this.#seen.get(scopedKey);
      }

      // 2. Commit to a server seed, derive ids and per-leg txRefs.
      const { seed, commitment } = newServerSeed();
      const nonce = this.#nextNonce(seedKey);
      const roundId = `${req.operatorId}-r-${this.#seq++}-${nonce}`;
      const stakeTxRef = `${roundId}:bet`;
      const payoutTxRef = `${roundId}:win`;

      // 3. Debit the stake via the operator's wallet. A decline is a normal
      //    rejection (insufficient funds / limit / excluded) — propagate it;
      //    no outcome is generated.
      await wallet.debit({
        operatorId: req.operatorId,
        playerId: req.playerId,
        amount: req.bet.stake,
        txRef: stakeTxRef,
        roundId,
        gameName: req.gameName,
      });

      // From here the stake is held on the operator's books. Anything that
      // fails BEFORE we commit a result must roll the debit back.
      let outcome, rec;
      try {
        // 4. Generate the deterministic outcome and settle (pure).
        const roll = generate(seed, req.clientSeed, nonce);
        outcome = game.settle(req.bet, roll);

        // 5. Record to the provider reconciliation ledger. Accounts are
        //    namespaced by operator; this is a GGR mirror, NOT custody.
        rec = this.#ledger.append({
          roundId,
          memo: `${req.operatorId}:${game.name}`,
          postings: this.#postings(req, outcome),
        });
      } catch (err) {
        await this.#rollback(wallet, req, stakeTxRef);
        throw err;
      }

      // 6. On a win, credit the payout. A failed credit is RETRIED (idempotent
      //    on payoutTxRef), never rolled back — the player won.
      let settlement = outcome.won ? "credited" : "no_payout";
      if (outcome.won) {
        settlement = await this.#creditWithRetry(wallet, req, outcome, payoutTxRef, roundId);
      }

      const result = {
        roundId,
        operatorId: req.operatorId,
        idempotencyKey: req.idempotencyKey,
        outcome,
        settlement, // "credited" | "no_payout" | "payout_pending"
        commitment,
        serverSeed: seed.toString("hex"),
        clientSeed: req.clientSeed,
        nonce,
        ledgerSeq: rec.journal.seq,
        payoutTxRef: outcome.won ? payoutTxRef : null,
      };
      this.#seen.set(scopedKey, result);
      this.#fingerprints.set(scopedKey, fp);
      return result;
    } finally {
      mu.release();
    }
  }

  // GGR-mirror postings: the player's operator-side balance is mirrored against
  // the operator's GGR pool. Stake moves player -> ggr; a win moves ggr ->
  // player. Per-operator GGR = sum over op:<id>:ggr. (PLATFORM_PRIMER.md §5)
  #postings(req, outcome) {
    const player = `op:${req.operatorId}:player:${req.playerId}`;
    const ggr = `op:${req.operatorId}:ggr`;
    const postings = [
      { account: player, amount: req.bet.stake.neg() },
      { account: ggr, amount: req.bet.stake },
    ];
    if (outcome.won) {
      postings.push(
        { account: ggr, amount: outcome.payout.neg() },
        { account: player, amount: outcome.payout },
      );
    }
    return postings;
  }

  async #creditWithRetry(wallet, req, outcome, payoutTxRef, roundId) {
    let lastErr;
    for (let attempt = 0; attempt <= this.#creditRetries; attempt++) {
      try {
        await wallet.credit({
          operatorId: req.operatorId,
          playerId: req.playerId,
          amount: outcome.payout,
          txRef: payoutTxRef, // idempotent: a retry confirms or applies once
          roundId,
          gameName: req.gameName,
        });
        return "credited";
      } catch (err) {
        lastErr = err;
      }
    }
    // Out of retries. The round still happened and is recorded; the payout is
    // owed. Surface it so an out-of-band sweep can re-credit the same txRef.
    void lastErr;
    return "payout_pending";
  }

  async #rollback(wallet, req, stakeTxRef) {
    try {
      await wallet.rollback({
        operatorId: req.operatorId,
        playerId: req.playerId,
        asset: req.bet.stake.asset,
        betTxRef: stakeTxRef,
        txRef: `${stakeTxRef}:rollback`,
      });
    } catch {
      // Rollback itself failed (boundary down). The orphaned debit is swept by
      // reconciliation (EDGE_CASES.md §14). Don't mask the original error.
    }
  }

  #enforceTenantLimits(tenant, bet) {
    const lim = tenant.limitFor(bet.stake.asset);
    if (!lim) return; // game defaults apply (already checked in validateBet)
    const v = bet.stake.value;
    if (v < lim.min) throw new Error("stake below operator minimum");
    if (v > lim.max) throw new Error("stake above operator maximum");
  }

  #nextNonce(seedKey) {
    const n = this.#nonces.get(seedKey) ?? 0n;
    this.#nonces.set(seedKey, n + 1n);
    return n;
  }

  #lockFor(lockKey) {
    let mu = this.#locks.get(lockKey);
    if (!mu) {
      mu = new AsyncMutex();
      this.#locks.set(lockKey, mu);
    }
    return mu;
  }
}

function fingerprint(req) {
  return [
    req.operatorId,
    req.playerId,
    req.gameName,
    req.bet.stake.asset.key,
    req.bet.stake.value.toString(),
    req.bet.target,
    req.clientSeed,
  ].join("|");
}

// AsyncMutex — a Promise-based lock. Node is single-threaded, but await yields,
// so without a lock async ops interleave. One instance per (operator, player);
// production replaces this with DB row locks / a transaction.
class AsyncMutex {
  #locked = false;
  #queue = [];

  acquire() {
    if (!this.#locked) {
      this.#locked = true;
      return Promise.resolve();
    }
    return new Promise((resolve) => this.#queue.push(resolve));
  }

  release() {
    const next = this.#queue.shift();
    if (next) next();
    else this.#locked = false;
  }
}

export { WalletDeclinedError };
