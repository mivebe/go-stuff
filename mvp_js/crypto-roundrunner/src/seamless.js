// seamless.js
//
// The seamless (transfer) wallet boundary. In a game-provider platform the
// OPERATOR custodies funds; the provider calls the operator's wallet API on
// every money event. We never hold a player balance.
//
// The contract is three idempotent operations, each carrying a provider-unique
// `txRef` that the operator dedupes on:
//
//   debit({ playerId, amount, txRef, roundId, gameName })   -> { balance }
//   credit({ playerId, amount, txRef, roundId, gameName })  -> { balance }
//   rollback({ playerId, asset, betTxRef, txRef })           -> { balance }
//
// `InMemoryOperatorWallet` below stands in for a real operator's wallet service
// (HTTP/gRPC in production). It models the two behaviours that make the
// boundary hard:
//
//   1. Idempotency on `txRef` — a retried call returns the original result,
//      never a second money movement (EDGE_CASES.md §14).
//   2. Failure injection — `faults` lets a test/demo force a decline or a
//      transient error on a chosen leg, so the rollback / retry paths are
//      exercised, not just described.
//
// Amounts cross the boundary as asset-tagged `Amount`s (see asset.js). A real
// adapter maps the operator's currency to our (symbol, chain) identity at the
// edge; here the operator speaks the same Amount type for clarity.

import { Amount } from "./asset.js";

export class WalletDeclinedError extends Error {
  constructor(playerId, requested, available) {
    super(`wallet declined: ${playerId} has ${available}, needs ${requested}`);
    this.name = "WalletDeclinedError";
    this.playerId = playerId;
    this.requested = requested;
    this.available = available;
  }
}

// A transient, retryable failure of the operator's wallet (timeout, 503, lost
// response). The provider must NOT assume "didn't happen" — the safe move is to
// retry the same txRef, which is idempotent. (EDGE_CASES.md §14)
export class WalletUnavailableError extends Error {
  constructor(op, message = "operator wallet unavailable") {
    super(`${op}: ${message}`);
    this.name = "WalletUnavailableError";
    this.op = op;
  }
}

export class InMemoryOperatorWallet {
  // playerId -> Map(assetKey -> Amount)
  #balances = new Map();
  // txRef -> { kind, result }  (idempotency across the boundary)
  #applied = new Map();
  // optional fault injector: (op, ctx) => void | throw
  #faults;

  /** @param {{ faults?: (op: string, ctx: object) => void }} [opts] */
  constructor({ faults } = {}) {
    this.#faults = faults ?? null;
  }

  /** Operator-side top-up (player deposited, KYC'd, confirmed — all the
   *  operator's concern). Not part of the seamless contract; demo/test seed. */
  fund(playerId, amount) {
    this.#creditInternal(playerId, amount);
  }

  balance(playerId, asset) {
    const m = this.#balances.get(playerId);
    if (!m) return new Amount(asset, 0n);
    return m.get(asset.key) ?? new Amount(asset, 0n);
  }

  // ---- seamless contract -------------------------------------------------

  async debit({ playerId, amount, txRef, roundId, gameName }) {
    if (this.#applied.has(txRef)) return this.#applied.get(txRef).result; // dedupe
    this.#maybeFault("debit", { playerId, txRef, roundId, gameName });

    const current = this.balance(playerId, amount.asset);
    if (current.lt(amount)) {
      // A decline is a normal outcome, not a transient error: do NOT record it
      // under txRef (a later retry with funds should be allowed to succeed).
      throw new WalletDeclinedError(playerId, amount, current);
    }
    this.#subInternal(playerId, amount);
    const result = { balance: this.balance(playerId, amount.asset) };
    // Record the debited amount so a later rollback can restore exactly it.
    this.#applied.set(txRef, { kind: "debit", result, debited: amount });
    return result;
  }

  async credit({ playerId, amount, txRef, roundId, gameName }) {
    if (this.#applied.has(txRef)) return this.#applied.get(txRef).result; // dedupe
    this.#maybeFault("credit", { playerId, txRef, roundId, gameName });

    this.#creditInternal(playerId, amount);
    const result = { balance: this.balance(playerId, amount.asset) };
    this.#applied.set(txRef, { kind: "credit", result });
    return result;
  }

  /** Reverse a previously-applied debit. Idempotent on its own txRef; a no-op
   *  if the original debit was never applied (e.g. it had been declined). */
  async rollback({ playerId, asset, betTxRef, txRef }) {
    if (this.#applied.has(txRef)) return this.#applied.get(txRef).result; // dedupe
    this.#maybeFault("rollback", { playerId, betTxRef, txRef });

    const original = this.#applied.get(betTxRef);
    if (original && original.kind === "debit") {
      // Restore exactly the stake the original debit removed.
      this.#creditInternal(playerId, original.debited);
    }
    const result = { balance: this.balance(playerId, asset) };
    this.#applied.set(txRef, { kind: "rollback", result });
    return result;
  }

  // ---- internals ---------------------------------------------------------

  #maybeFault(op, ctx) {
    if (this.#faults) this.#faults(op, ctx);
  }

  #creditInternal(playerId, amount) {
    let m = this.#balances.get(playerId);
    if (!m) {
      m = new Map();
      this.#balances.set(playerId, m);
    }
    const cur = m.get(amount.asset.key) ?? new Amount(amount.asset, 0n);
    m.set(amount.asset.key, cur.add(amount));
  }

  #subInternal(playerId, amount) {
    const m = this.#balances.get(playerId);
    const cur = m.get(amount.asset.key);
    m.set(amount.asset.key, cur.sub(amount));
  }
}
