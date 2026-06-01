import { test } from "node:test";
import assert from "node:assert/strict";
import { Amount, Asset, BTC, ETH, USDT_TRON } from "../src/asset.js";

test("Amount requires BigInt value", () => {
  // The kind of mistake Number-for-money would silently allow.
  assert.throws(() => new Amount(BTC, 100), TypeError);
  assert.throws(() => new Amount(BTC, "100"), TypeError);
});

test("arithmetic across different assets is rejected", () => {
  const oneBtc = new Amount(BTC, 10n ** 8n);
  const oneEth = new Amount(ETH, 10n ** 18n);
  assert.throws(() => oneBtc.add(oneEth), /asset mismatch/);
  assert.throws(() => oneBtc.sub(oneEth), /asset mismatch/);
});

test("same-asset arithmetic works at 18-decimal precision", () => {
  // 1 ETH + 0.5 ETH in wei. This is the case where Number would lose precision.
  const a = new Amount(ETH, 10n ** 18n);
  const b = new Amount(ETH, 5n * 10n ** 17n);
  assert.equal(a.add(b).value, 15n * 10n ** 17n);
});

test("mulBps floors (truncates toward zero for non-negative)", () => {
  // 333 micro-USDT × 1.96x = 652.68 — floored to 652.
  const stake = new Amount(USDT_TRON, 333n);
  assert.equal(stake.mulBps(19_600n).value, 652n);
});

test("toJSON/fromJSON roundtrip preserves precision", () => {
  const big = new Amount(ETH, 12345678901234567890n);
  const round = Amount.fromJSON(JSON.parse(JSON.stringify(big)));
  assert.equal(round.value, big.value);
  assert.equal(round.asset.key, big.asset.key);
});

test("identity: same symbol on different chains is different asset", () => {
  const usdtTron = USDT_TRON;
  const usdtEth = new Asset({ symbol: "USDT", chain: "ethereum", decimals: 6 });
  assert.notEqual(usdtTron.key, usdtEth.key);
  assert.throws(() =>
    new Amount(usdtTron, 100n).add(new Amount(usdtEth, 100n)),
  );
});
