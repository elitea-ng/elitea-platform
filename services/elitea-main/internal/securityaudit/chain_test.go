package securityaudit

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestChainHashIsSHA256OfPrevThenCanonical(t *testing.T) {
	prev := Hash{1, 2, 3}
	canonical := []byte(`{"a":1}`)
	want := sha256.Sum256(append(append([]byte{}, prev[:]...), canonical...))
	if got := ChainHash(prev, canonical); got != Hash(want) {
		t.Fatalf("ChainHash = %x, want %x", got, want)
	}
	if ChainHash(ZeroHash, canonical) == ChainHash(prev, canonical) {
		t.Fatal("prev is not part of the hash")
	}
}

// buildChain makes n rows of partition 42 the way the appender writes them.
func buildChain(t *testing.T, n int) []StoredRow {
	t.Helper()
	rows := make([]StoredRow, 0, n)
	prev := ZeroHash
	for i := 1; i <= n; i++ {
		event := sampleEvent()
		event.ID = fmt.Sprintf("0192f0c1-7b6e-7c3a-8f00-%012d", i)
		canonical, err := event.Canonical(int64(i))
		if err != nil {
			t.Fatal(err)
		}
		hash := ChainHash(prev, canonical)
		rows = append(rows, StoredRow{
			Seq: int64(100 + i), PartitionKey: "42", PartitionSeq: int64(i), EventID: event.ID,
			Event: jsonbLike(t, canonical), PrevHash: bytes.Clone(prev[:]), Hash: bytes.Clone(hash[:]),
		})
		prev = hash
	}
	return rows
}

func verifyRows(rows []StoredRow, from int64, anchor Hash) (*ChainVerifier, *Break) {
	verifier := NewChainVerifier("42", from, anchor)
	for _, row := range rows {
		if broken := verifier.Check(row); broken != nil {
			return verifier, broken
		}
	}
	return verifier, nil
}

func TestVerifierAcceptsAnIntactChain(t *testing.T) {
	rows := buildChain(t, 20)
	verifier, broken := verifyRows(rows, 1, ZeroHash)
	if broken != nil {
		t.Fatal(broken)
	}
	if verifier.Checked() != 20 {
		t.Fatalf("checked %d", verifier.Checked())
	}
	// From the middle, anchored on the previous row's hash.
	var anchor Hash
	copy(anchor[:], rows[9].Hash)
	if _, broken := verifyRows(rows[10:], 11, anchor); broken != nil {
		t.Fatal(broken)
	}
}

func TestVerifierFindsTheExactEditedSeq(t *testing.T) {
	for _, edited := range []int{0, 6, 19} {
		rows := buildChain(t, 20)
		var generic map[string]any
		_ = json.Unmarshal(rows[edited].Event, &generic)
		generic["data"].(map[string]any)["effect"] = "allow"
		rows[edited].Event, _ = json.Marshal(generic)

		_, broken := verifyRows(rows, 1, ZeroHash)
		if broken == nil {
			t.Fatalf("edit of row %d not found", edited)
		}
		if broken.PartitionSeq != int64(edited+1) || broken.Seq != rows[edited].Seq {
			t.Fatalf("break at %d (seq %d), want %d (seq %d)", broken.PartitionSeq, broken.Seq, edited+1, rows[edited].Seq)
		}
		if !strings.Contains(broken.Reason, "hash does not match") {
			t.Fatalf("reason %q", broken.Reason)
		}
	}
}

func TestVerifierFindsARecomputedHashByItsSuccessor(t *testing.T) {
	// An attacker who edits row 5 and recomputes its hash breaks row 6's
	// prev_hash link.
	rows := buildChain(t, 10)
	event := sampleEvent()
	event.ID = rows[4].EventID
	event.Effect = EffectAllow
	canonical, _ := event.Canonical(5)
	var prev Hash
	copy(prev[:], rows[3].Hash)
	forged := ChainHash(prev, canonical)
	rows[4].Event, rows[4].Hash = canonical, forged[:]

	_, broken := verifyRows(rows, 1, ZeroHash)
	if broken == nil || broken.PartitionSeq != 6 || !strings.Contains(broken.Reason, "prev_hash") {
		t.Fatalf("break = %v, want prev_hash break at 6", broken)
	}
}

func TestVerifierFindsAMissingRow(t *testing.T) {
	rows := buildChain(t, 10)
	rows = append(rows[:3], rows[4:]...)
	_, broken := verifyRows(rows, 1, ZeroHash)
	if broken == nil || broken.PartitionSeq != 4 || !strings.Contains(broken.Reason, "missing") {
		t.Fatalf("break = %v, want missing row 4", broken)
	}
}

func TestVerifierFindsAMovedEnvelope(t *testing.T) {
	rows := buildChain(t, 3)
	rows[1].EventID = rows[0].EventID
	_, broken := verifyRows(rows, 1, ZeroHash)
	if broken == nil || broken.PartitionSeq != 2 {
		t.Fatalf("break = %v, want 2", broken)
	}
	rows = buildChain(t, 3)
	rows[2].PrevHash = rows[2].PrevHash[:31]
	if _, broken := verifyRows(rows, 1, ZeroHash); broken == nil || broken.PartitionSeq != 3 {
		t.Fatalf("short hash: break = %v", broken)
	}
	rows = buildChain(t, 3)
	rows[0].PartitionKey = "43"
	if _, broken := verifyRows(rows, 1, ZeroHash); broken == nil || broken.PartitionSeq != 1 {
		t.Fatalf("foreign partition: break = %v", broken)
	}
}
