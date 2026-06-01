package ledger

import (
	"errors"
	"math/big"
	"testing"

	"roundrunner/internal/asset"
)

func TestRejectsSingleLegPosting(t *testing.T) {
	l := New()
	_, err := l.Append(Journal{
		RoundID:  "x",
		Memo:     "x",
		Postings: []Posting{{Account: "a", Amount: asset.NewInt(asset.BTC, 100)}},
	})
	var unb *UnbalancedError
	if !errors.As(err, &unb) {
		t.Fatalf("want UnbalancedError, got %v", err)
	}
}

func TestPerAssetBalanceBTCCannotNetAgainstUSDT(t *testing.T) {
	// A journal that nets to zero in AGGREGATE but not per-asset must be rejected.
	l := New()
	_, err := l.Append(Journal{
		RoundID: "x", Memo: "x",
		Postings: []Posting{
			{Account: "a", Amount: asset.NewInt(asset.BTC, 100)},
			{Account: "b", Amount: asset.NewInt(asset.USDT_TRON, -100)},
		},
	})
	var unb *UnbalancedError
	if !errors.As(err, &unb) {
		t.Fatalf("want UnbalancedError, got %v", err)
	}
}

func TestMultiAssetBalancedJournalAccepted(t *testing.T) {
	l := New()
	if _, err := l.Append(Journal{
		RoundID: "x", Memo: "x",
		Postings: []Posting{
			{Account: "a", Amount: asset.NewInt(asset.BTC, -50)},
			{Account: "b", Amount: asset.NewInt(asset.BTC, 50)},
			{Account: "a", Amount: asset.NewInt(asset.USDT_TRON, -200)},
			{Account: "b", Amount: asset.NewInt(asset.USDT_TRON, 200)},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got := l.Balance("b", asset.BTC).Value(); got.Cmp(big.NewInt(50)) != 0 {
		t.Fatalf("b BTC: got %s want 50", got)
	}
	if got := l.Balance("b", asset.USDT_TRON).Value(); got.Cmp(big.NewInt(200)) != 0 {
		t.Fatalf("b USDT: got %s want 200", got)
	}
}

// This test lives inside package ledger so it can reach into a sealed record
// and mutate it — exactly what an attacker (or a buggy migration) would do.
func TestTamperDetection(t *testing.T) {
	l := New()
	for _, amt := range []int64{10, 5} {
		if _, err := l.Append(Journal{
			RoundID: "x", Memo: "x",
			Postings: []Posting{
				{Account: "a", Amount: asset.NewInt(asset.BTC, -amt)},
				{Account: "b", Amount: asset.NewInt(asset.BTC, amt)},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Verify(); err != nil {
		t.Fatalf("clean ledger should verify: %v", err)
	}
	l.records[0].Journal.Postings[0] = Posting{Account: "a", Amount: asset.NewInt(asset.BTC, -999)}
	if err := l.Verify(); err == nil {
		t.Fatal("verify should detect tampering")
	}
}
