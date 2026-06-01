// Package ledger is an append-only, tamper-evident, double-entry ledger.
//
// Three properties matter for a real-money platform:
//
//   - Double-entry, PER ASSET: every journal is a set of postings that must sum
//     to zero for EACH asset independently. A journal that nets to zero in
//     aggregate but moves BTC against USDT is "money created from thin air" and
//     is rejected at write time. You cannot cancel a BTC debit with a USDT
//     credit.
//   - Append-only: records are only ever added, never updated or deleted. The
//     ledger is both the source of truth and the audit trail.
//   - Tamper-evident: each record stores the hash of the previous record plus
//     its own contents (a hash chain). Altering any past record breaks every
//     hash after it, so tampering is detectable by re-walking the chain.
//
// In the game-provider topology this ledger is a RECONCILIATION / GGR mirror,
// not a custody record — the operator's wallet holds the funds. The mechanism
// is unchanged; only its role is.
package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"roundrunner/internal/asset"
)

// Posting is one leg of a journal entry. The amount is signed: a credit is
// positive, a debit is negative. Each posting carries its own asset.
type Posting struct {
	Account string       `json:"account"`
	Amount  asset.Amount `json:"amount"`
}

// Journal is a balanced (per asset) set of postings recorded atomically.
type Journal struct {
	Seq       uint64    `json:"seq"`
	RoundID   string    `json:"round_id"`
	Memo      string    `json:"memo"`
	Postings  []Posting `json:"postings"`
	Timestamp time.Time `json:"timestamp"`
}

// Record is a Journal sealed into the hash chain.
type Record struct {
	Journal  Journal `json:"journal"`
	PrevHash string  `json:"prev_hash"`
	Hash     string  `json:"hash"`
}

// UnbalancedError is returned when postings for some asset do not sum to zero.
type UnbalancedError struct {
	AssetKey string
	Sum      string
}

func (e *UnbalancedError) Error() string {
	return fmt.Sprintf("ledger: postings for %s sum to %s, expected 0", e.AssetKey, e.Sum)
}

// Ledger is an in-memory hash-chained ledger. In production the records live in
// an append-only table and the chain head is persisted; the shape is the same.
type Ledger struct {
	mu      sync.Mutex
	records []Record
}

// New returns an empty ledger.
func New() *Ledger { return &Ledger{} }

// Append validates the per-asset double-entry invariant, seals the journal into
// the chain, and returns the sealed record. Safe for concurrent use.
func (l *Ledger) Append(j Journal) (Record, error) {
	// Sum postings per asset key; every asset must net to exactly zero.
	sums := map[string]asset.Amount{}
	for _, p := range j.Postings {
		k := p.Amount.Asset().Key()
		if cur, ok := sums[k]; ok {
			sums[k] = cur.Add(p.Amount)
		} else {
			sums[k] = p.Amount
		}
	}
	for k, s := range sums {
		if !s.IsZero() {
			return Record{}, &UnbalancedError{AssetKey: k, Sum: s.Value().String()}
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	prevHash := "GENESIS"
	if n := len(l.records); n > 0 {
		prevHash = l.records[n-1].Hash
	}
	j.Seq = uint64(len(l.records))
	if j.Timestamp.IsZero() {
		j.Timestamp = time.Now().UTC()
	}

	rec := Record{Journal: j, PrevHash: prevHash}
	rec.Hash = hashRecord(prevHash, j)
	l.records = append(l.records, rec)
	return rec, nil
}

// Balance reconstructs an account's balance for one asset by replaying the
// ledger. The ledger is the source of truth; any cached balance must reconcile.
func (l *Ledger) Balance(account string, a asset.Asset) asset.Amount {
	l.mu.Lock()
	defer l.mu.Unlock()
	total := asset.Zero(a)
	for _, r := range l.records {
		for _, p := range r.Journal.Postings {
			if p.Account == account && p.Amount.Asset().Key() == a.Key() {
				total = total.Add(p.Amount)
			}
		}
	}
	return total
}

// Verify re-walks the chain and confirms no record has been altered.
func (l *Ledger) Verify() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	prevHash := "GENESIS"
	for i, r := range l.records {
		if r.PrevHash != prevHash {
			return fmt.Errorf("ledger: record %d prev_hash mismatch", i)
		}
		if r.Hash != hashRecord(prevHash, r.Journal) {
			return fmt.Errorf("ledger: record %d has been tampered with", i)
		}
		prevHash = r.Hash
	}
	return nil
}

// Records returns a copy of the chain for inspection.
func (l *Ledger) Records() []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Record, len(l.records))
	copy(out, l.records)
	return out
}

func hashRecord(prevHash string, j Journal) string {
	// Canonical serialisation: encoding/json emits struct fields in declaration
	// order, and asset.Amount marshals to a stable {asset, v-string} shape, so
	// two semantically identical journals hash identically. Without that
	// stability the tamper-evidence breaks before it starts.
	body, _ := json.Marshal(j)
	h := sha256.New()
	h.Write([]byte(prevHash))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}
