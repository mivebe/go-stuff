// Package game holds game configuration and the pure outcome->payout logic.
//
// Everything here is deterministic and side-effect free: given a config, a bet,
// and a roll, it returns the same Result every time. That purity is what lets
// us SIMULATE and VERIFY a game's RTP before it ships against real money (see
// the RTP convergence demo) without touching wallets, ledgers, or the network.
//
// The game itself is "dice over": the player wins if the roll lands at or above
// their Target. A higher target is less likely but pays a bigger multiplier.
package game

import (
	"errors"
	"fmt"

	"roundrunner/internal/asset"
	"roundrunner/internal/rng"
)

var (
	ErrBetTooSmall = errors.New("game: stake below minimum")
	ErrBetTooLarge = errors.New("game: stake above maximum")
	ErrBadTarget   = errors.New("game: target out of range")
	ErrWrongAsset  = errors.New("game: bet asset does not match game asset")
)

// Bet is the player's selection. They win if roll >= Target.
type Bet struct {
	Stake  asset.Amount
	Target rng.Roll // win if roll >= Target; Target in [1, RollSpace)
}

// Result is the deterministic outcome of applying a roll to a bet.
type Result struct {
	Roll          rng.Roll
	Won           bool
	MultiplierBps int64
	Payout        asset.Amount // amount returned to the player (zero on a loss)
}

// Game is what the round engine depends on. Both the real Config and the
// duck-typed "faulty" games used in failure-path tests satisfy it. Settle
// returns an error so a settlement fault can be modelled (and force a rollback)
// — the JS sibling expresses the same thing with a thrown exception.
type Game interface {
	Name() string
	ValidateBet(Bet) error
	Settle(Bet, rng.Roll) (Result, error)
}

// Config captures the economics of a game — the knobs a game-configuration
// system exposes per game (and, via TenantConfig, per operator). Fields are
// unexported and set through NewConfig so a Config is effectively immutable
// once built, mirroring the frozen GameConfig class in the JS sibling.
type Config struct {
	name         string
	asset        asset.Asset
	houseEdgeBps int64
	minBet       asset.Amount
	maxBet       asset.Amount
}

// Spec is the constructor input for a Config.
type Spec struct {
	Name         string
	Asset        asset.Asset
	HouseEdgeBps int64 // e.g. 200 == 2% edge => 98% RTP
	MinBet       asset.Amount
	MaxBet       asset.Amount
}

// NewConfig validates and builds a Config. MinBet/MaxBet must use the
// configured asset.
func NewConfig(s Spec) (Config, error) {
	if s.MinBet.Asset().Key() != s.Asset.Key() || s.MaxBet.Asset().Key() != s.Asset.Key() {
		return Config{}, fmt.Errorf("game: minBet/maxBet must use the configured asset %s", s.Asset.Key())
	}
	return Config{
		name:         s.Name,
		asset:        s.Asset,
		houseEdgeBps: s.HouseEdgeBps,
		minBet:       s.MinBet,
		maxBet:       s.MaxBet,
	}, nil
}

func (c Config) Name() string        { return c.name }
func (c Config) Asset() asset.Asset  { return c.asset }
func (c Config) HouseEdgeBps() int64 { return c.houseEdgeBps }

// ValidateBet enforces the asset match and the configured limits.
func (c Config) ValidateBet(b Bet) error {
	if b.Stake.Asset().Key() != c.asset.Key() {
		return fmt.Errorf("%w: %s vs %s", ErrWrongAsset, b.Stake.Asset().Key(), c.asset.Key())
	}
	if b.Stake.Lt(c.minBet) {
		return ErrBetTooSmall
	}
	if c.maxBet.Lt(b.Stake) {
		return ErrBetTooLarge
	}
	if b.Target < 1 || b.Target >= rng.RollSpace {
		return ErrBadTarget
	}
	return nil
}

// MultiplierBps sets the payout multiplier so that RTP == (1 - houseEdge),
// independent of the target the player picks:
//
//	winProb    = (RollSpace - Target) / RollSpace
//	multiplier = (1 - houseEdge) / winProb
//	RTP        = winProb * multiplier = (1 - houseEdge)
//
// Computed in basis points and integers throughout.
func (c Config) MultiplierBps(target rng.Roll) int64 {
	winOutcomes := int64(rng.RollSpace) - int64(target)
	return (10_000 - c.houseEdgeBps) * int64(rng.RollSpace) / winOutcomes
}

// Settle applies a roll to a bet. Pure function — no I/O. The error return is
// always nil here; it exists to satisfy the Game interface, whose contract
// allows a settlement fault.
func (c Config) Settle(b Bet, roll rng.Roll) (Result, error) {
	won := roll >= b.Target
	mult := c.MultiplierBps(b.Target)
	res := Result{Roll: roll, Won: won, MultiplierBps: mult, Payout: asset.Zero(b.Stake.Asset())}
	if won {
		res.Payout = b.Stake.MulBps(mult)
	}
	return res, nil
}
