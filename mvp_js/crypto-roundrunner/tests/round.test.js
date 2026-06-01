import { test } from "node:test";
import assert from "node:assert/strict";
import { Amount, USDT_TRON } from "../src/asset.js";
import { GameConfig } from "../src/game.js";
import { Ledger } from "../src/ledger.js";
import { TenantConfig, OperatorRegistry } from "../src/tenant.js";
import { InMemoryOperatorWallet } from "../src/seamless.js";
import { RoundEngine, ReplayMismatchError } from "../src/round.js";

const STAKE = new Amount(USDT_TRON, 1_000_000n);

function newSetup({ faults, creditRetries } = {}) {
  const game = new GameConfig({
    name: "dice",
    asset: USDT_TRON,
    houseEdgeBps: 200n,
    minBet: new Amount(USDT_TRON, 1n),
    maxBet: new Amount(USDT_TRON, 1_000_000_000n),
  });
  const games = new Map([[game.name, game]]);
  const wallet = new InMemoryOperatorWallet({ faults });
  wallet.fund("p", new Amount(USDT_TRON, 100_000_000n));
  const registry = new OperatorRegistry();
  registry.register(
    new TenantConfig({
      operatorId: "op1",
      enabledGames: ["dice"],
      allowedAssets: [USDT_TRON.key],
      callbackSecret: "secret",
    }),
    wallet,
  );
  const ledger = new Ledger();
  return {
    game,
    wallet,
    ledger,
    engine: new RoundEngine({ games, registry, ledger, creditRetries }),
  };
}

function req(over = {}) {
  return {
    operatorId: "op1",
    playerId: "p",
    gameName: "dice",
    clientSeed: "s",
    idempotencyKey: "k1",
    bet: { stake: STAKE, target: 5000 },
    ...over,
  };
}

test("idempotent replay does not double-charge", async () => {
  const { wallet, engine } = newSetup();
  const r1 = await engine.execute(req());
  const after = wallet.balance("p", USDT_TRON).value;
  const r2 = await engine.execute(req());
  assert.equal(wallet.balance("p", USDT_TRON).value, after);
  assert.equal(r1.roundId, r2.roundId);
});

test("same idempotency key with a different bet is rejected", async () => {
  const { engine } = newSetup();
  await engine.execute(req());
  await assert.rejects(
    () => engine.execute(req({ bet: { stake: new Amount(USDT_TRON, 2_000_000n), target: 5000 } })),
    ReplayMismatchError,
  );
});

test("a debit decline propagates and moves no money", async () => {
  const { wallet, engine } = newSetup();
  const before = wallet.balance("p", USDT_TRON).value;
  await assert.rejects(
    () => engine.execute(req({ bet: { stake: new Amount(USDT_TRON, 999_000_000n), target: 5000 } })),
    /declined/,
  );
  assert.equal(wallet.balance("p", USDT_TRON).value, before);
});

test("settlement failure after debit rolls the stake back", async () => {
  // GameConfig is frozen, so we register a duck-typed game whose settle throws
  // after the debit has already succeeded — exercising the rollback path.
  const faulty = {
    name: "faulty",
    validateBet() {},
    settle() {
      throw new Error("settlement fault");
    },
  };
  const games = new Map([["faulty", faulty]]);
  const wallet = new InMemoryOperatorWallet();
  wallet.fund("p", new Amount(USDT_TRON, 100_000_000n));
  const registry = new OperatorRegistry();
  registry.register(
    new TenantConfig({
      operatorId: "op1",
      enabledGames: ["faulty"],
      allowedAssets: [USDT_TRON.key],
      callbackSecret: "s",
    }),
    wallet,
  );
  const engine = new RoundEngine({ games, registry, ledger: new Ledger() });
  const before = wallet.balance("p", USDT_TRON).value;
  await assert.rejects(
    () => engine.execute(req({ gameName: "faulty", idempotencyKey: "rb" })),
    /settlement fault/,
  );
  assert.equal(wallet.balance("p", USDT_TRON).value, before, "stake restored by rollback");
});

test("a credit failure marks payout pending, never rolls back the win", async () => {
  let fail = true;
  // creditRetries: 0 so the single faulted credit is not silently retried away.
  const { wallet, engine } = newSetup({
    creditRetries: 0,
    faults(op) {
      if (op === "credit" && fail) {
        fail = false;
        throw new Error("wallet timeout");
      }
    },
  });
  // target 1 -> deterministic win, so a payout is always attempted.
  let pending = null;
  for (let i = 0; i < 20 && !pending; i++) {
    const r = await engine.execute(
      req({ idempotencyKey: `c-${i}`, bet: { stake: STAKE, target: 1 } }),
    );
    if (r.outcome.won && r.settlement === "payout_pending") pending = r;
  }
  assert.ok(pending, "a winning round whose credit faulted should be payout_pending");
  assert.ok(pending.payoutTxRef, "pending payout exposes its txRef for re-credit");
  // The stake was debited and NOT rolled back: the player won, the payout is
  // merely owed. Balance must never go negative.
  assert.ok(wallet.balance("p", USDT_TRON).value >= 0n);
});

test("concurrent executes on one account preserve invariants", async () => {
  const { wallet, ledger, engine } = newSetup(); // p funded with 100 USDT
  // Two 60-USDT stakes against a 100-USDT balance: at most one can clear on
  // debit alone, so a naive engine would overdraw. The per-account lock + the
  // operator wallet's debit check together prevent it.
  const stake = new Amount(USDT_TRON, 60_000_000n);
  await Promise.allSettled([
    engine.execute(req({ idempotencyKey: "a", bet: { stake, target: 5000 } })),
    engine.execute(req({ idempotencyKey: "b", bet: { stake, target: 5000 } })),
  ]);
  assert.ok(wallet.balance("p", USDT_TRON).value >= 0n, "balance never negative");
  ledger.verify();
});
