// Package asset models crypto money: an integer count of an asset's smallest
// indivisible unit, tagged with the Asset it belongs to.
//
// Why not int64 (as the original fiat engine used)? Because crypto amounts
// routinely exceed it. 1 ETH in wei is 10^18; int64 holds ~9.2 × 10^18, so a
// single ETH fits but ten ETH (10^19) overflows, and intermediate products in
// fee math overflow far sooner. The JavaScript sibling uses BigInt for exactly
// this reason (Number tops out at 2^53 − 1 ≈ 9 × 10^15). The faithful Go
// equivalent is math/big.Int: arbitrary precision, no silent wraparound.
//
// Every amount is tagged with its Asset so the runtime can reject "BTC + USDT"
// as the bug it is. Asset identity is (symbol, chain): USDT-on-Tron and
// USDT-on-Ethereum are DIFFERENT assets even though the symbol matches.
package asset

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// Asset identifies a crypto asset by (symbol, chain) and records how many
// decimal places its smallest unit represents. Decimals vary wildly across
// assets (BTC 8, ETH 18, USDC 6), which is precisely the class of bug you
// guard against by tagging amounts.
type Asset struct {
	Symbol   string
	Chain    string
	Decimals int
}

// Key is the asset's identity. "USDT on Tron" and "USDT on Ethereum" produce
// different keys and therefore cannot be added together.
func (a Asset) Key() string { return a.Symbol + ":" + a.Chain }

// Format renders a base-unit integer for humans, trimming trailing zeros.
func (a Asset) Format(v *big.Int) string {
	if a.Decimals == 0 {
		return fmt.Sprintf("%s %s", v.String(), a.Symbol)
	}
	sign := ""
	abs := new(big.Int).Abs(v)
	if v.Sign() < 0 {
		sign = "-"
	}
	div := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(a.Decimals)), nil)
	whole := new(big.Int).Quo(abs, div)
	frac := new(big.Int).Mod(abs, div)

	fracStr := frac.String()
	if len(fracStr) < a.Decimals { // left-pad to the full decimal width
		fracStr = strings.Repeat("0", a.Decimals-len(fracStr)) + fracStr
	}
	fracStr = strings.TrimRight(fracStr, "0")
	if fracStr == "" {
		fracStr = "0"
	}
	return fmt.Sprintf("%s%s.%s %s", sign, whole.String(), fracStr, a.Symbol)
}

// Well-known assets. Note the wildly different decimals.
var (
	BTC       = Asset{Symbol: "BTC", Chain: "bitcoin", Decimals: 8}
	ETH       = Asset{Symbol: "ETH", Chain: "ethereum", Decimals: 18}
	USDT_TRON = Asset{Symbol: "USDT", Chain: "tron", Decimals: 6}
	USDT_ETH  = Asset{Symbol: "USDT", Chain: "ethereum", Decimals: 6}
	USDC_ETH  = Asset{Symbol: "USDC", Chain: "ethereum", Decimals: 6}
)

var assetsByKey = func() map[string]Asset {
	m := map[string]Asset{}
	for _, a := range []Asset{BTC, ETH, USDT_TRON, USDT_ETH, USDC_ETH} {
		m[a.Key()] = a
	}
	return m
}()

// Lookup resolves an asset by its key (e.g. "USDT:tron"). Used when hydrating
// amounts from JSON, where only the key survives the round-trip.
func Lookup(key string) (Asset, error) {
	a, ok := assetsByKey[key]
	if !ok {
		return Asset{}, fmt.Errorf("asset: unknown asset %q", key)
	}
	return a, nil
}

// Amount is an (asset, big.Int) pair. All arithmetic enforces asset equality.
// The value is never shared by reference across instances: every operation
// returns a fresh Amount over a fresh big.Int, so an Amount behaves like an
// immutable value despite big.Int being a pointer type.
type Amount struct {
	asset Asset
	value *big.Int
}

// New builds an Amount from an asset and a big.Int, copying the value so the
// caller cannot later mutate it out from under us. A nil value means zero.
func New(a Asset, v *big.Int) Amount {
	if v == nil {
		return Amount{asset: a, value: new(big.Int)}
	}
	return Amount{asset: a, value: new(big.Int).Set(v)}
}

// NewInt is a convenience constructor for amounts that fit in an int64.
func NewInt(a Asset, v int64) Amount { return Amount{asset: a, value: big.NewInt(v)} }

// Zero is the additive identity for an asset.
func Zero(a Asset) Amount { return Amount{asset: a, value: new(big.Int)} }

// MustParse builds an Amount from a base-units decimal string (e.g. "1000").
func MustParse(a Asset, s string) Amount {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic(fmt.Sprintf("asset: cannot parse %q as base units", s))
	}
	return Amount{asset: a, value: v}
}

func (a Amount) Asset() Asset { return a.asset }

// Value returns a copy of the underlying integer; mutating it cannot affect
// the Amount.
func (a Amount) Value() *big.Int { return new(big.Int).Set(a.value) }

func (a Amount) Add(b Amount) Amount {
	a.requireSameAsset(b)
	return Amount{a.asset, new(big.Int).Add(a.value, b.value)}
}

func (a Amount) Sub(b Amount) Amount {
	a.requireSameAsset(b)
	return Amount{a.asset, new(big.Int).Sub(a.value, b.value)}
}

func (a Amount) Neg() Amount { return Amount{a.asset, new(big.Int).Neg(a.value)} }

func (a Amount) IsZero() bool     { return a.value.Sign() == 0 }
func (a Amount) IsNegative() bool { return a.value.Sign() < 0 }

func (a Amount) Lt(b Amount) bool {
	a.requireSameAsset(b)
	return a.value.Cmp(b.value) < 0
}

// MulBps multiplies by a ratio in basis points (1 bps = 1/10000). Used to apply
// a payout multiplier while keeping the math in integers. Stakes are
// non-negative here, so big.Int's truncate-toward-zero division (Quo) equals
// floor — the house-favourable rounding direction. The rounding policy must be
// EXPLICIT: multiplying a negative amount is refused rather than silently
// papered over, mirroring the JS sibling.
func (a Amount) MulBps(bps int64) Amount {
	if a.value.Sign() < 0 {
		panic("asset: MulBps on a negative amount: define your rounding policy first")
	}
	r := new(big.Int).Mul(a.value, big.NewInt(bps))
	r.Quo(r, big.NewInt(10_000))
	return Amount{a.asset, r}
}

// String renders the amount for humans.
func (a Amount) String() string { return a.asset.Format(a.value) }

// jsonAmount is the stable wire form. big.Int has no native JSON number that
// preserves arbitrary precision, so the value travels as a decimal STRING. The
// ledger hash depends on this being stable.
type jsonAmount struct {
	Asset string `json:"asset"`
	V     string `json:"v"`
}

func (a Amount) MarshalJSON() ([]byte, error) {
	return json.Marshal(jsonAmount{Asset: a.asset.Key(), V: a.value.String()})
}

func (a *Amount) UnmarshalJSON(b []byte) error {
	var raw jsonAmount
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	as, err := Lookup(raw.Asset)
	if err != nil {
		return err
	}
	v, ok := new(big.Int).SetString(raw.V, 10)
	if !ok {
		return fmt.Errorf("asset: cannot parse amount value %q", raw.V)
	}
	a.asset = as
	a.value = v
	return nil
}

func (a Amount) requireSameAsset(b Amount) {
	if a.asset.Key() != b.asset.Key() {
		// An asset mismatch is a programming bug, not a runtime condition the
		// caller can recover from — fail loud, exactly like the JS sibling's
		// thrown TypeError.
		panic(fmt.Sprintf("asset: asset mismatch: %s vs %s", a.asset.Key(), b.asset.Key()))
	}
}
