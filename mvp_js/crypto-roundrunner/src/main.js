// main.js
//
// Self-narrating simulation of the game-provider platform. One engine serves
// two operators (tenants). It shows: tenant resolution, per-operator config and
// isolation, the seamless wallet bet/win path, a rollback when settlement
// fails after a debit, tenant-scoped idempotency, provable fairness, and
// per-operator GGR read off the reconciliation ledger.

import { USDT_TRON, Amount } from "./asset.js";
import { GameConfig } from "./game.js";
import { Ledger } from "./ledger.js";
import { TenantConfig, OperatorRegistry, UnknownOperatorError } from "./tenant.js";
import { InMemoryOperatorWallet } from "./seamless.js";
import { RoundEngine } from "./round.js";
import { verify } from "./rng.js";

// --- Games the provider offers (the platform's catalogue) ---
const dice = new GameConfig({
  name: "dice-over",
  asset: USDT_TRON,
  houseEdgeBps: 200n,
  minBet: new Amount(USDT_TRON, 100_000n), //   0.10 USDT
  maxBet: new Amount(USDT_TRON, 1_000_000_000n), // 1000.00 USDT
});
const games = new Map([[dice.name, dice]]);

// --- Two operators (tenants), each with its own wallet, secret, limits ---
const registry = new OperatorRegistry();

const acmeWallet = new InMemoryOperatorWallet();
registry.register(
  new TenantConfig({
    operatorId: "acme",
    displayName: "Acme Casino",
    enabledGames: ["dice-over"],
    allowedAssets: [USDT_TRON.key],
    callbackSecret: "acme-secret-key",
    jurisdiction: "curacao",
  }),
  acmeWallet,
);

const betcoWallet = new InMemoryOperatorWallet();
registry.register(
  new TenantConfig({
    operatorId: "betco",
    displayName: "BetCo",
    enabledGames: ["dice-over"],
    allowedAssets: [USDT_TRON.key],
    // BetCo runs a tighter per-operator max than the game default.
    limits: { [USDT_TRON.key]: { min: 100_000n, max: 50_000_000n } },
    callbackSecret: "betco-secret-key",
    jurisdiction: "mga",
  }),
  betcoWallet,
);

const ledger = new Ledger();
const engine = new RoundEngine({ games, registry, ledger, creditRetries: 1 });

// Operator-side balances (the operator custodies these; we never do).
acmeWallet.fund("alice", new Amount(USDT_TRON, 1_000_000_000n)); // 1000 USDT
betcoWallet.fund("alice", new Amount(USDT_TRON, 1_000_000_000n)); // a DIFFERENT alice

const stake = new Amount(USDT_TRON, 10_000_000n); // 10 USDT

// --- Tenant isolation: same idempotency key + same player name, two operators ---
console.log("== tenant isolation (same key 'k1', same player name 'alice') ==");
for (const op of ["acme", "betco"]) {
  const res = await engine.execute({
    operatorId: op,
    playerId: "alice",
    gameName: "dice-over",
    clientSeed: "alice-seed",
    idempotencyKey: "k1",
    bet: { stake, target: 5000 },
  });
  console.log(
    `${op.padEnd(6)} round=${res.roundId.padEnd(18)} roll=${String(res.outcome.roll).padStart(4)} ` +
      `${res.outcome.won ? "WON " : "LOST"} settlement=${res.settlement}`,
  );
}
console.log(
  `acme alice wallet:  ${acmeWallet.balance("alice", USDT_TRON)}\n` +
    `betco alice wallet: ${betcoWallet.balance("alice", USDT_TRON)}  (different person, independent balance)\n`,
);

// --- Unknown operator fails closed ---
console.log("== unknown operator rejected ==");
try {
  await engine.execute({
    operatorId: "ghost",
    playerId: "x",
    gameName: "dice-over",
    clientSeed: "s",
    idempotencyKey: "k",
    bet: { stake, target: 5000 },
  });
} catch (e) {
  console.log(`${e instanceof UnknownOperatorError ? "rejected" : "?"}: ${e.message}\n`);
}

// --- Idempotent replay is tenant-scoped ---
console.log("== idempotent replay (acme / k1) ==");
const before = acmeWallet.balance("alice", USDT_TRON).value;
const replay = await engine.execute({
  operatorId: "acme",
  playerId: "alice",
  gameName: "dice-over",
  clientSeed: "alice-seed",
  idempotencyKey: "k1",
  bet: { stake, target: 5000 },
});
console.log(
  `balance unchanged after replay? ${acmeWallet.balance("alice", USDT_TRON).value === before}` +
    `   (stable round id: ${replay.roundId})\n`,
);

// --- Provable fairness: verify the replayed round from the revealed seed ---
console.log("== provable fairness (verify acme/k1) ==");
const ok = verify({
  commitment: replay.commitment,
  serverSeed: Buffer.from(replay.serverSeed, "hex"),
  clientSeed: replay.clientSeed,
  nonce: replay.nonce,
  claimedRoll: replay.outcome.roll,
});
console.log(`revealed seed recomputes the roll? ${ok}\n`);

// --- Rollback: a debit succeeds, settlement throws -> stake is restored ---
console.log("== rollback on post-debit failure (acme) ==");
// Dedicated wallet/engine for this scenario: round ids are unique per engine
// instance (a single-instance simplification — production uses a UUID or a DB
// sequence), so we don't reuse the wallet above and collide on a txRef.
const rbWallet = new InMemoryOperatorWallet();
rbWallet.fund("alice", new Amount(USDT_TRON, 100_000_000n)); // 100 USDT
const balPreRollback = rbWallet.balance("alice", USDT_TRON).value;
// A duck-typed game whose settle throws AFTER the debit, simulating an internal
// settlement fault. The engine must roll the operator-side debit back.
const faultyGame = {
  name: "faulty-dice",
  validateBet: dice.validateBet.bind(dice),
  settle() {
    throw new Error("settlement engine fault");
  },
};
const rbRegistry = new OperatorRegistry();
rbRegistry.register(
  new TenantConfig({
    operatorId: "acme",
    enabledGames: ["faulty-dice"],
    allowedAssets: [USDT_TRON.key],
    callbackSecret: "k",
  }),
  rbWallet,
);
const rbEngine = new RoundEngine({
  games: new Map([["faulty-dice", faultyGame]]),
  registry: rbRegistry,
  ledger: new Ledger(),
});
try {
  await rbEngine.execute({
    operatorId: "acme",
    playerId: "alice",
    gameName: "faulty-dice",
    clientSeed: "alice-seed",
    idempotencyKey: "rollback-1",
    bet: { stake, target: 5000 },
  });
} catch (e) {
  console.log(`round aborted: ${e.message}`);
}
console.log(
  `stake rolled back? ${rbWallet.balance("alice", USDT_TRON).value === balPreRollback}` +
    `   (balance restored to pre-bet)\n`,
);

// --- Payout pending: credit leg times out -> retried, not rolled back ---
console.log("== payout-pending (betco, first credit leg faults) ==");
// The operator wallet fails the FIRST credit it sees, then recovers. With no
// in-band retry (creditRetries: 0) the engine records the round and marks the
// payout pending — owed, to be re-credited out of band via the same txRef.
let failNextCredit = true;
const betcoWalletFaulty = new InMemoryOperatorWallet({
  faults(op) {
    if (op === "credit" && failNextCredit) {
      failNextCredit = false;
      throw new Error("operator wallet timeout");
    }
  },
});
const registry2 = new OperatorRegistry();
registry2.register(
  new TenantConfig({
    operatorId: "betco",
    enabledGames: ["dice-over"],
    allowedAssets: [USDT_TRON.key],
    callbackSecret: "betco-secret-key",
  }),
  betcoWalletFaulty,
);
const engine2 = new RoundEngine({
  games,
  registry: registry2,
  ledger: new Ledger(),
  creditRetries: 0, // no in-band retry, so we observe payout_pending
});
betcoWalletFaulty.fund("bob", new Amount(USDT_TRON, 1_000_000_000n));
let pendingSeen = false;
for (let i = 0; i < 12 && !pendingSeen; i++) {
  const res = await engine2.execute({
    operatorId: "betco",
    playerId: "bob",
    gameName: "dice-over",
    clientSeed: "bob-seed",
    idempotencyKey: `payout-${i}`,
    bet: { stake, target: 100 }, // ~99% win chance -> a payout is attempted
  });
  if (res.outcome.won) {
    console.log(
      `round ${res.roundId} WON, settlement=${res.settlement}` +
        (res.settlement === "payout_pending"
          ? `  -> owed, re-credit via txRef ${res.payoutTxRef} (NOT rolled back)`
          : ""),
    );
    if (res.settlement === "payout_pending") pendingSeen = true;
  }
}
console.log();

// --- Per-operator GGR off the reconciliation ledger ---
console.log("== per-operator GGR (provider reconciliation mirror) ==");
for (const op of registry.operatorIds()) {
  const ggr = ledger.balance(`op:${op}:ggr`, USDT_TRON);
  console.log(`${op.padEnd(6)} GGR = ${ggr}  (stakes - payouts; revenue-share basis)`);
}
ledger.verify();
console.log(`\nledger hash chain intact across ${ledger.records().length} records`);

// --- RTP convergence for one operator (proves the game math pre-launch) ---
console.log("\n== RTP convergence (acme, 50k rounds) ==");
const rtpWallet = new InMemoryOperatorWallet();
const rtpRegistry = new OperatorRegistry();
rtpRegistry.register(
  new TenantConfig({
    operatorId: "acme",
    enabledGames: ["dice-over"],
    allowedAssets: [USDT_TRON.key],
    callbackSecret: "k",
  }),
  rtpWallet,
);
const rtpEngine = new RoundEngine({ games, registry: rtpRegistry, ledger: new Ledger() });
rtpWallet.fund("bot", new Amount(USDT_TRON, 1_000_000_000_000n)); // 1,000,000 USDT
const stake1 = new Amount(USDT_TRON, 1_000_000n); // 1 USDT
let staked = 0n;
let returned = 0n;
const N = 50_000;
for (let i = 0; i < N; i++) {
  const res = await rtpEngine.execute({
    operatorId: "acme",
    playerId: "bot",
    gameName: "dice-over",
    clientSeed: "bot-seed",
    idempotencyKey: `bot-${i}`,
    bet: { stake: stake1, target: 5000 },
  });
  staked += stake1.value;
  returned += res.outcome.payout.value;
}
const rtpPct = (Number(returned) / Number(staked)) * 100;
console.log(`staked=${staked} returned=${returned} realised RTP=${rtpPct.toFixed(2)}% (target 98.00%)`);
