// rng.js
//
// Provably-fair commit-reveal outcome generation. The casino is a crypto
// product, so this isn't just an internal correctness tool — it's also a
// USER-FACING TRUST FEATURE. Players verify rounds independently. Treat it
// like a contract: the commitment is published before the round, the seed is
// revealed afterwards, and the same inputs always produce the same roll.

import { randomBytes, createHmac, createHash } from "node:crypto";

// Outcome space: roll in [0, ROLL_SPACE). 10000 == basis-point granularity.
export const ROLL_SPACE = 10_000;

// Generate a fresh server seed and publish its commitment. Use the OS CSPRNG
// (`crypto.randomBytes`) — never `Math.random`, which is not cryptographically
// secure and is fully predictable from a few samples.
export function newServerSeed() {
  const seed = randomBytes(32);
  return { seed, commitment: commit(seed) };
}

export function commit(seed) {
  return createHash("sha256").update(seed).digest("hex");
}

// Deterministic roll from (serverSeed, clientSeed, nonce). Same inputs ALWAYS
// produce the same output — that's what makes a round auditable.
//
// nonce is a BigInt because round counts can exceed Number.MAX_SAFE_INTEGER
// over the long life of a platform; we want one type, not two.
export function generate(serverSeed, clientSeed, nonce) {
  if (typeof nonce !== "bigint") {
    throw new TypeError(`nonce must be BigInt, got ${typeof nonce}`);
  }
  const mac = createHmac("sha256", serverSeed);
  mac.update(`${clientSeed}:${nonce.toString()}`);
  const digest = mac.digest();
  // First 4 bytes -> uint32 -> [0, ROLL_SPACE). Modulo bias is ~2 × 10^-6 here
  // (ROLL_SPACE / 2^32), negligible; rejection sampling is the strict version.
  const n = digest.readUInt32BE(0);
  return n % ROLL_SPACE;
}

// Verify a previously-played round given the REVEALED server seed. Both the
// commitment check and the roll recomputation must pass. A player or auditor
// runs this; the server doesn't have to be trusted to do it for them.
export function verify({ commitment, serverSeed, clientSeed, nonce, claimedRoll }) {
  if (commit(serverSeed) !== commitment) return false;
  return generate(serverSeed, clientSeed, nonce) === claimedRoll;
}
