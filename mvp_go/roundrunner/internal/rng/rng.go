// Package rng implements provably-fair outcome generation using a
// commit-reveal scheme.
//
// The flow, and why each step exists:
//
//  1. Before the round, the server generates a random serverSeed and publishes
//     Commitment = SHA256(serverSeed). The hash reveals nothing about the seed
//     but BINDS the server to it: the server can no longer choose the seed
//     after seeing the player's input.
//  2. The player supplies a clientSeed. Because the server can't predict it,
//     the server can't have pre-picked a favourable serverSeed for this round.
//  3. Outcome = HMAC-SHA256(serverSeed, clientSeed || ":" || nonce). This is
//     DETERMINISTIC: identical inputs always produce the same roll. Determinism
//     is what makes a round auditable and replayable.
//  4. After the round the server REVEALS serverSeed. Anyone can check that
//     SHA256(serverSeed) == Commitment and recompute the roll. If both hold,
//     the result was fixed before the player committed and was not tampered
//     with afterwards. That is "provably fair".
package rng

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// RollSpace is the size of the outcome space. A Roll is a uniform integer in
// [0, RollSpace), i.e. basis points of the unit interval.
const RollSpace = 10000

// Roll is a single generated outcome in [0, RollSpace).
type Roll uint32

// NewServerSeed returns a fresh 32-byte server seed and its commitment.
func NewServerSeed() (seed []byte, commitment string, err error) {
	seed = make([]byte, 32)
	if _, err = rand.Read(seed); err != nil {
		return nil, "", fmt.Errorf("rng: read entropy: %w", err)
	}
	return seed, Commit(seed), nil
}

// Commit returns SHA256(seed) as hex - the value published before the round.
func Commit(seed []byte) string {
	sum := sha256.Sum256(seed)
	return hex.EncodeToString(sum[:])
}

// Generate derives the deterministic roll for a seed pair and nonce.
//
// The nonce is a uint64. The JS sibling uses a BigInt here because its Number
// type loses integer precision past 2^53; Go's uint64 holds ~1.8 × 10^19
// rounds without loss, which outlives any real platform, so no big.Int is
// needed for the counter.
func Generate(serverSeed []byte, clientSeed string, nonce uint64) Roll {
	mac := hmac.New(sha256.New, serverSeed)
	fmt.Fprintf(mac, "%s:%d", clientSeed, nonce)
	digest := mac.Sum(nil)

	// Map the first 4 bytes to a uint32, then into [0, RollSpace). The modulo
	// introduces a negligible bias (RollSpace / 2^32 ~= 2e-6); a system that
	// truly cares would use rejection sampling. Kept simple here on purpose.
	n := binary.BigEndian.Uint32(digest[:4])
	return Roll(n % RollSpace)
}

// Verify re-runs the commitment check and roll derivation. A player or auditor
// calls this with the revealed seed to confirm the round was fair.
func Verify(commitment string, serverSeed []byte, clientSeed string, nonce uint64, claimed Roll) bool {
	if Commit(serverSeed) != commitment {
		return false
	}
	return Generate(serverSeed, clientSeed, nonce) == claimed
}
