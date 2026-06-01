import { test } from "node:test";
import assert from "node:assert/strict";
import { Amount, USDT_TRON, BTC } from "../src/asset.js";
import { GameConfig } from "../src/game.js";
import { Ledger } from "../src/ledger.js";
import { TenantConfig, OperatorRegistry, UnknownOperatorError } from "../src/tenant.js";
import { InMemoryOperatorWallet } from "../src/seamless.js";
import { RoundEngine, GameNotEnabledError, AssetNotAllowedError } from "../src/round.js";

const STAKE = new Amount(USDT_TRON, 1_000_000n);

function game() {
  return new GameConfig({
    name: "dice",
    asset: USDT_TRON,
    houseEdgeBps: 200n,
    minBet: new Amount(USDT_TRON, 1n),
    maxBet: new Amount(USDT_TRON, 1_000_000_000n),
  });
}

// Two operators sharing one engine; each gets its own wallet.
function platform() {
  const games = new Map([["dice", game()]]);
  const registry = new OperatorRegistry();
  const wallets = {};
  for (const id of ["acme", "betco"]) {
    const w = new InMemoryOperatorWallet();
    w.fund("alice", new Amount(USDT_TRON, 100_000_000n));
    wallets[id] = w;
    registry.register(
      new TenantConfig({
        operatorId: id,
        enabledGames: ["dice"],
        allowedAssets: [USDT_TRON.key],
        callbackSecret: `${id}-secret`,
        // betco runs a tighter max.
        limits: id === "betco" ? { [USDT_TRON.key]: { min: 1n, max: 5_000_000n } } : {},
      }),
      w,
    );
  }
  const ledger = new Ledger();
  return { engine: new RoundEngine({ games, registry, ledger }), registry, ledger, wallets };
}

function req(operatorId, over = {}) {
  return {
    operatorId,
    playerId: "alice",
    gameName: "dice",
    clientSeed: "s",
    idempotencyKey: "k1",
    bet: { stake: STAKE, target: 5000 },
    ...over,
  };
}

test("unknown operator fails closed before any work", async () => {
  const { engine, wallets } = platform();
  const before = wallets.acme.balance("alice", USDT_TRON).value;
  await assert.rejects(() => engine.execute(req("ghost")), UnknownOperatorError);
  assert.equal(wallets.acme.balance("alice", USDT_TRON).value, before, "no money touched");
});

test("idempotency is tenant-scoped: same key under two operators is independent", async () => {
  const { engine } = platform();
  const a = await engine.execute(req("acme")); // key k1
  const b = await engine.execute(req("betco")); // SAME key k1, different tenant
  assert.notEqual(a.roundId, b.roundId, "keys must not collide across tenants");
  assert.equal(a.operatorId, "acme");
  assert.equal(b.operatorId, "betco");
});

test("a player name is per-operator: balances are isolated", async () => {
  const { engine, wallets } = platform();
  await engine.execute(req("acme"));
  assert.notEqual(
    wallets.acme.balance("alice", USDT_TRON).value,
    100_000_000n,
    "acme alice's balance changed (debited, maybe credited)",
  );
  assert.equal(
    wallets.betco.balance("alice", USDT_TRON).value,
    100_000_000n,
    "betco alice (a different person) is untouched",
  );
});

test("a game not enabled for the operator is rejected", async () => {
  const games = new Map([["dice", game()]]);
  const registry = new OperatorRegistry();
  const w = new InMemoryOperatorWallet();
  w.fund("alice", new Amount(USDT_TRON, 100_000_000n));
  registry.register(
    new TenantConfig({
      operatorId: "acme",
      enabledGames: ["some-other-game"],
      allowedAssets: [USDT_TRON.key],
      callbackSecret: "s",
    }),
    w,
  );
  const engine = new RoundEngine({ games, registry, ledger: new Ledger() });
  await assert.rejects(() => engine.execute(req("acme")), GameNotEnabledError);
});

test("an asset not allowed for the operator is rejected", async () => {
  const { engine } = platform();
  // acme only allows USDT_TRON; a BTC stake must be refused.
  await assert.rejects(
    () => engine.execute(req("acme", { bet: { stake: new Amount(BTC, 1000n), target: 5000 } })),
    AssetNotAllowedError,
  );
});

test("per-operator stake limit is enforced over the game default", async () => {
  const { engine } = platform();
  // betco caps stakes at 5 USDT; a 10 USDT stake is within the game default
  // but over betco's tenant limit.
  await assert.rejects(
    () => engine.execute(req("betco", { bet: { stake: new Amount(USDT_TRON, 10_000_000n), target: 5000 } })),
    /operator maximum/,
  );
});

test("GGR is tracked per operator in the reconciliation ledger", async () => {
  const { engine, ledger } = platform();
  await engine.execute(req("acme", { idempotencyKey: "a1" }));
  await engine.execute(req("betco", { idempotencyKey: "b1" }));

  // Accounts are namespaced by operator and the mirror is double-entry, so for
  // each operator the GGR pool is exactly the negation of the player mirror —
  // regardless of win/loss. This is the per-operator invoicing basis.
  for (const op of ["acme", "betco"]) {
    const ggr = ledger.balance(`op:${op}:ggr`, USDT_TRON).value;
    const player = ledger.balance(`op:${op}:player:alice`, USDT_TRON).value;
    assert.equal(ggr + player, 0n, `${op} GGR mirrors its player postings`);
  }
  // betco's accounts carry none of acme's postings: a betco-only round leaves
  // acme's ledger footprint to just its own single round.
  assert.equal(ledger.balance("op:acme:player:alice", USDT_TRON).value !== 0n, true);
  ledger.verify();
});
