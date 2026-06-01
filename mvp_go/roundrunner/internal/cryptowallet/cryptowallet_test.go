package cryptowallet

import (
	"errors"
	"testing"

	"roundrunner/internal/asset"
)

func TestPendingDepositNotBettableUntilConfirmed(t *testing.T) {
	w := NewInMemory()
	deposit := asset.NewInt(asset.BTC, 50_000_000) // 0.5 BTC
	w.NotifyDeposit("alice", deposit, "tx1", 3)

	// Seen but not spendable.
	if got := w.Balance("alice", asset.BTC).Value().Int64(); got != 0 {
		t.Fatalf("balance before confirmation: got %d want 0", got)
	}
	if pending := w.PendingFor("alice"); len(pending) != 1 {
		t.Fatalf("want 1 pending deposit, got %d", len(pending))
	}

	// Two confirmations: still below the threshold of 3.
	w.AdvanceConfirmations("tx1", 2)
	if got := w.Balance("alice", asset.BTC).Value().Int64(); got != 0 {
		t.Fatalf("balance at 2/3 confirmations: got %d want 0", got)
	}

	// Threshold met: the deposit becomes spendable and leaves the pending pool.
	w.AdvanceConfirmations("tx1", 3)
	if got := w.Balance("alice", asset.BTC).Value().Int64(); got != 50_000_000 {
		t.Fatalf("balance after confirmation: got %d want 50000000", got)
	}
	if pending := w.PendingFor("alice"); len(pending) != 0 {
		t.Fatalf("confirmed deposit should leave the pending pool, got %d", len(pending))
	}
}

func TestDebitIsIdempotentAndGuardsFunds(t *testing.T) {
	w := NewInMemory()
	w.Fund("alice", asset.NewInt(asset.USDT_TRON, 1_000_000))

	if err := w.Debit("alice", asset.NewInt(asset.USDT_TRON, 400_000), "ref1"); err != nil {
		t.Fatal(err)
	}
	// Replaying the same ref is a no-op, not a second deduction.
	if err := w.Debit("alice", asset.NewInt(asset.USDT_TRON, 400_000), "ref1"); err != nil {
		t.Fatal(err)
	}
	if got := w.Balance("alice", asset.USDT_TRON).Value().Int64(); got != 600_000 {
		t.Fatalf("after idempotent debit: got %d want 600000", got)
	}

	// Overdraw is refused.
	err := w.Debit("alice", asset.NewInt(asset.USDT_TRON, 999_000), "ref2")
	var insufficient *InsufficientFundsError
	if !errors.As(err, &insufficient) {
		t.Fatalf("want InsufficientFundsError, got %v", err)
	}
}
