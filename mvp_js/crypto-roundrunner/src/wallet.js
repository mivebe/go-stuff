// wallet.js
//
// In a crypto-casino-plus-wallet product the wallet is INTERNAL (you own both
// products), but the engine should still depend on an interface — so unit
// tests don't need real chains and so the runtime can swap the impl for a
// segregated cold/hot architecture without ceremony.
//
// Two things make this wallet "crypto" rather than "fiat":
//
//   1. Multi-asset balances. An account holds BTC AND ETH AND USDT
//      simultaneously, indexed by Asset.key.
//   2. Pending vs confirmed. Deposits don't move into the bettable balance
//      until they have N confirmations. Before that they sit in a pending
//      pool, where they are visible to the user but unavailable for play.
//      Reorgs (chain reorganisations) can roll back a confirmed deposit —
//      handle by writing a REVERSAL journal, never by mutating history.

import { Amount } from "./asset.js";

export class InsufficientFundsError extends Error {
  constructor(account, requested, available) {
    super(
      `insufficient funds: account ${account} has ${available} but needs ${requested}`,
    );
    this.name = "InsufficientFundsError";
    this.account = account;
    this.requested = requested;
    this.available = available;
  }
}

export class InMemoryCryptoWallet {
  // account -> Map(assetKey -> Amount)
  #confirmed = new Map();
  // txHash -> { account, amount, required, current }
  #pending = new Map();
  // idempotency refs already applied
  #applied = new Set();

  // ------- Operational (called by chain listeners, not by the engine) -------

  /** A new on-chain deposit was seen. It joins the pending pool with zero
   *  confirmations and is NOT credited yet. */
  notifyDeposit({ account, amount, txHash, requiredConfirmations }) {
    if (this.#pending.has(txHash)) return; // dedupe on tx hash
    this.#pending.set(txHash, {
      account,
      amount,
      required: requiredConfirmations,
      current: 0,
    });
  }

  /** Chain reports a new block height for a pending deposit. When confirmations
   *  meet the threshold, the deposit moves into the spendable balance. */
  advanceConfirmations(txHash, confirmations) {
    const dep = this.#pending.get(txHash);
    if (!dep) return;
    dep.current = confirmations;
    if (dep.current >= dep.required) {
      this.#creditInternal(dep.account, dep.amount);
      this.#pending.delete(txHash);
    }
  }

  /** Hook the engine never touches: seed initial balance for demos/tests. */
  fund(account, amount) {
    this.#creditInternal(account, amount);
  }

  // ------- Engine-facing wallet interface -------

  async debit(account, amount, ref) {
    if (this.#applied.has(ref)) return; // idempotent replay
    const current = this.balance(account, amount.asset);
    if (current.lt(amount)) {
      throw new InsufficientFundsError(account, amount, current);
    }
    this.#subInternal(account, amount);
    this.#applied.add(ref);
  }

  async credit(account, amount, ref) {
    if (this.#applied.has(ref)) return;
    this.#creditInternal(account, amount);
    this.#applied.add(ref);
  }

  balance(account, asset) {
    const m = this.#confirmed.get(account);
    if (!m) return new Amount(asset, 0n);
    return m.get(asset.key) ?? new Amount(asset, 0n);
  }

  pendingFor(account) {
    const out = [];
    for (const [txHash, dep] of this.#pending) {
      if (dep.account === account) out.push({ txHash, ...dep });
    }
    return out;
  }

  // ------- internals -------

  #creditInternal(account, amount) {
    let m = this.#confirmed.get(account);
    if (!m) {
      m = new Map();
      this.#confirmed.set(account, m);
    }
    const cur = m.get(amount.asset.key) ?? new Amount(amount.asset, 0n);
    m.set(amount.asset.key, cur.add(amount));
  }

  #subInternal(account, amount) {
    const m = this.#confirmed.get(account);
    const cur = m.get(amount.asset.key);
    m.set(amount.asset.key, cur.sub(amount));
  }
}
