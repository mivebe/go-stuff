import { test } from "node:test";
import assert from "node:assert/strict";
import { Amount, BTC, USDT_TRON } from "../src/asset.js";
import { Ledger, UnbalancedJournalError } from "../src/ledger.js";

test("rejects single-leg posting", () => {
  const l = new Ledger();
  assert.throws(
    () =>
      l.append({
        roundId: "x",
        memo: "x",
        postings: [{ account: "a", amount: new Amount(BTC, 100n) }],
      }),
    UnbalancedJournalError,
  );
});

test("per-asset balance: BTC cannot net against USDT", () => {
  // A journal that nets to zero in AGGREGATE but not per-asset must be rejected.
  const l = new Ledger();
  assert.throws(
    () =>
      l.append({
        roundId: "x",
        memo: "x",
        postings: [
          { account: "a", amount: new Amount(BTC, 100n) },
          { account: "b", amount: new Amount(USDT_TRON, -100n) },
        ],
      }),
    UnbalancedJournalError,
  );
});

test("multi-asset balanced journal is accepted", () => {
  const l = new Ledger();
  l.append({
    roundId: "x",
    memo: "x",
    postings: [
      { account: "a", amount: new Amount(BTC, -50n) },
      { account: "b", amount: new Amount(BTC, 50n) },
      { account: "a", amount: new Amount(USDT_TRON, -200n) },
      { account: "b", amount: new Amount(USDT_TRON, 200n) },
    ],
  });
  assert.equal(l.balance("b", BTC).value, 50n);
  assert.equal(l.balance("b", USDT_TRON).value, 200n);
});

test("tamper detection: altering a sealed record fails verify", () => {
  const l = new Ledger();
  l.append({
    roundId: "x",
    memo: "x",
    postings: [
      { account: "a", amount: new Amount(BTC, -10n) },
      { account: "b", amount: new Amount(BTC, 10n) },
    ],
  });
  l.append({
    roundId: "y",
    memo: "y",
    postings: [
      { account: "a", amount: new Amount(BTC, -5n) },
      { account: "b", amount: new Amount(BTC, 5n) },
    ],
  });
  l.verify(); // clean
  const recs = l.records();
  recs[0].journal.postings[0] = {
    account: "a",
    amount: new Amount(BTC, -999n),
  };
  assert.throws(() => l.verify(), /tampered/);
});
