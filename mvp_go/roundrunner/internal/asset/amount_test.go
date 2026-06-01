package asset

import (
	"encoding/json"
	"math/big"
	"testing"
)

// mustPanic runs fn and fails unless it panics. The asset-mismatch and
// negative-mulBps invariants are programming bugs, so the code panics (the JS
// sibling throws); we assert that here.
func mustPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s: expected a panic, got none", name)
		}
	}()
	fn()
}

func TestArithmeticAcrossDifferentAssetsIsRejected(t *testing.T) {
	oneBTC := New(BTC, new(big.Int).Exp(big.NewInt(10), big.NewInt(8), nil))
	oneETH := New(ETH, new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	mustPanic(t, "add", func() { oneBTC.Add(oneETH) })
	mustPanic(t, "sub", func() { oneBTC.Sub(oneETH) })
	mustPanic(t, "lt", func() { oneBTC.Lt(oneETH) })
}

func TestSameAssetArithmeticAt18Decimals(t *testing.T) {
	// 1 ETH + 0.5 ETH in wei — the case where float/int64 would lose precision.
	a := New(ETH, new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	b := New(ETH, new(big.Int).Mul(big.NewInt(5), new(big.Int).Exp(big.NewInt(10), big.NewInt(17), nil)))
	want := new(big.Int).Mul(big.NewInt(15), new(big.Int).Exp(big.NewInt(10), big.NewInt(17), nil))
	if got := a.Add(b).Value(); got.Cmp(want) != 0 {
		t.Fatalf("1.5 ETH in wei: got %s want %s", got, want)
	}
}

func TestMulBpsFloors(t *testing.T) {
	// 333 micro-USDT × 1.96x = 652.68 — floored to 652.
	stake := NewInt(USDT_TRON, 333)
	if got := stake.MulBps(19_600).Value(); got.Cmp(big.NewInt(652)) != 0 {
		t.Fatalf("mulBps floor: got %s want 652", got)
	}
}

func TestMulBpsRejectsNegative(t *testing.T) {
	mustPanic(t, "mulBps negative", func() { NewInt(USDT_TRON, -1).MulBps(19_600) })
}

func TestJSONRoundtripPreservesPrecision(t *testing.T) {
	big1, _ := new(big.Int).SetString("12345678901234567890", 10) // > int64/2^53
	orig := New(ETH, big1)
	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var back Amount
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Value().Cmp(orig.Value()) != 0 {
		t.Fatalf("value drift: got %s want %s", back.Value(), orig.Value())
	}
	if back.Asset().Key() != orig.Asset().Key() {
		t.Fatalf("asset drift: got %s want %s", back.Asset().Key(), orig.Asset().Key())
	}
}

func TestIdentitySameSymbolDifferentChain(t *testing.T) {
	usdtTron := USDT_TRON
	usdtEth := Asset{Symbol: "USDT", Chain: "ethereum", Decimals: 6}
	if usdtTron.Key() == usdtEth.Key() {
		t.Fatal("USDT on Tron and Ethereum must have distinct keys")
	}
	mustPanic(t, "mismatch add", func() {
		New(usdtTron, big.NewInt(100)).Add(New(usdtEth, big.NewInt(100)))
	})
}
