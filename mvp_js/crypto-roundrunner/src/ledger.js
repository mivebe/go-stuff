// ledger.js
//
// Append-only, tamper-evident, double-entry ledger. The crypto-specific twist
// vs a fiat ledger: postings are MULTI-ASSET. A single journal may move BTC
// AND USDT, and balance-to-zero must hold PER ASSET (BTC sums to zero, USDT
// sums to zero) — you can't net a BTC debit against a USDT credit.

import { createHash } from "node:crypto";
import { Amount } from "./asset.js";

export class UnbalancedJournalError extends Error {
  constructor(assetKey, sum) {
    super(`ledger: postings for ${assetKey} sum to ${sum}, expected 0`);
    this.name = "UnbalancedJournalError";
  }
}

export class Ledger {
  #records = [];

  /**
   * Validate the per-asset double-entry invariant, seal the journal into the
   * hash chain, and return the sealed record. Atomic; concurrent callers in a
   * real system serialise on a DB transaction.
   */
  append(journal) {
    const sums = new Map(); // assetKey -> bigint sum
    for (const p of journal.postings) {
      if (!(p.amount instanceof Amount)) {
        throw new TypeError("posting.amount must be an Amount");
      }
      const k = p.amount.asset.key;
      sums.set(k, (sums.get(k) ?? 0n) + p.amount.value);
    }
    for (const [k, s] of sums) {
      if (s !== 0n) throw new UnbalancedJournalError(k, s);
    }

    const seq = this.#records.length;
    const prevHash = seq === 0 ? "GENESIS" : this.#records[seq - 1].hash;
    const sealed = {
      journal: {
        seq,
        roundId: journal.roundId,
        memo: journal.memo,
        postings: journal.postings,
        timestamp: journal.timestamp ?? new Date().toISOString(),
      },
      prevHash,
    };
    sealed.hash = hashRecord(prevHash, sealed.journal);
    this.#records.push(sealed);
    return sealed;
  }

  /** Per-asset balance for an account, reconstructed by replaying the ledger.
   *  The ledger is the source of truth; any cached balance must reconcile. */
  balance(account, asset) {
    let total = 0n;
    for (const r of this.#records) {
      for (const p of r.journal.postings) {
        if (p.account === account && p.amount.asset.key === asset.key) {
          total += p.amount.value;
        }
      }
    }
    return new Amount(asset, total);
  }

  /** Re-walk the chain. Throws if any record has been altered or relinked. */
  verify() {
    let prev = "GENESIS";
    for (let i = 0; i < this.#records.length; i++) {
      const r = this.#records[i];
      if (r.prevHash !== prev) {
        throw new Error(`ledger: record ${i} prev_hash mismatch`);
      }
      if (r.hash !== hashRecord(prev, r.journal)) {
        throw new Error(`ledger: record ${i} has been tampered with`);
      }
      prev = r.hash;
    }
  }

  records() {
    return [...this.#records];
  }
}

function hashRecord(prevHash, journal) {
  // Canonical serialisation: stable key order + BigInt as string. Without
  // canonicalisation, two semantically identical journals produce different
  // hashes — and your tamper-evidence is broken before it starts.
  return createHash("sha256")
    .update(prevHash)
    .update(canonicalize(journal))
    .digest("hex");
}

function canonicalize(value) {
  if (typeof value === "bigint") return JSON.stringify(value.toString());
  if (value === null) return "null";
  if (typeof value !== "object") return JSON.stringify(value);
  if (Array.isArray(value)) return "[" + value.map(canonicalize).join(",") + "]";
  if (typeof value.toJSON === "function") return canonicalize(value.toJSON());
  const keys = Object.keys(value).sort();
  return "{" + keys.map((k) => JSON.stringify(k) + ":" + canonicalize(value[k])).join(",") + "}";
}
