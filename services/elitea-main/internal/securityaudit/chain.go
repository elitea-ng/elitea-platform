package securityaudit

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"time"
)

// Hash is one chain link.
type Hash [32]byte

// ZeroHash is the prev_hash of the first event of a partition.
var ZeroHash Hash

// ChainHash is sha256(prev || canonical).
func ChainHash(prev Hash, canonical []byte) Hash {
	h := sha256.New()
	h.Write(prev[:])
	h.Write(canonical)
	var out Hash
	copy(out[:], h.Sum(nil))
	return out
}

// Record is one chained event as the outbox and the durable store hold it.
type Record struct {
	// Seq is the outbox seq: global, gap-tolerant, the relay's order.
	Seq          int64
	PartitionKey string
	// PartitionSeq is the chain position: 1, 2, 3, ... with no gap.
	PartitionSeq int64
	EventID      string
	EventType    EventType
	// Event is the stored event JSON. For a record the appender returns it
	// is the canonical bytes; read back from jsonb it is reformatted.
	Event     []byte
	PrevHash  Hash
	Hash      Hash
	CreatedAt time.Time
}

// Break is the first place a chain does not verify.
type Break struct {
	Seq          int64
	PartitionSeq int64
	Reason       string
}

func (b *Break) Error() string {
	return fmt.Sprintf("security audit chain broken at partition_seq %d (seq %d): %s", b.PartitionSeq, b.Seq, b.Reason)
}

// StoredRow is one row as the verifier reads it, before any decode. The
// hashes are raw so a wrong length is reported, not panicked on.
type StoredRow struct {
	Seq          int64
	PartitionKey string
	PartitionSeq int64
	EventID      string
	Event        []byte
	PrevHash     []byte
	Hash         []byte
}

// ChainVerifier checks one partition's rows in partition_seq order. It
// streams: feed rows with Check until one returns a Break.
type ChainVerifier struct {
	partitionKey string
	next         int64
	prev         Hash
	checked      int64
	canonical    Canonicalizer
}

// Canonicalizer returns the bytes a row's hash covers. It is what lets a
// chain whose rows are not security audit envelopes (centry.audit_events,
// shared migration 0206) reuse this verifier.
type Canonicalizer func(row StoredRow) ([]byte, error)

// NewChainVerifier starts at partition sequence from, whose prev_hash must
// be anchor. From 1 the anchor is ZeroHash. Rows are security audit
// envelopes: Check re-canonicalises row.Event and checks it against its row.
func NewChainVerifier(partitionKey string, from int64, anchor Hash) *ChainVerifier {
	return &ChainVerifier{partitionKey: partitionKey, next: from, prev: anchor}
}

// NewChainVerifierWith is NewChainVerifier for another chained table: the
// hash of a row covers canonical(row) instead of a security audit envelope.
func NewChainVerifierWith(partitionKey string, from int64, anchor Hash, canonical Canonicalizer) *ChainVerifier {
	return &ChainVerifier{partitionKey: partitionKey, next: from, prev: anchor, canonical: canonical}
}

// Checked is the number of rows that verified.
func (v *ChainVerifier) Checked() int64 { return v.checked }

// Next is the partition sequence the verifier expects next.
func (v *ChainVerifier) Next() int64 { return v.next }

// Check verifies one row against the chain so far.
func (v *ChainVerifier) Check(row StoredRow) *Break {
	fail := func(format string, args ...any) *Break {
		return &Break{Seq: row.Seq, PartitionSeq: row.PartitionSeq, Reason: fmt.Sprintf(format, args...)}
	}
	if row.PartitionKey != v.partitionKey {
		return fail("row belongs to partition %q", row.PartitionKey)
	}
	if row.PartitionSeq != v.next {
		// A missing row is reported at the position that is missing.
		return &Break{Seq: row.Seq, PartitionSeq: v.next, Reason: fmt.Sprintf("row missing: next stored row is partition_seq %d", row.PartitionSeq)}
	}
	if len(row.PrevHash) != len(Hash{}) || len(row.Hash) != len(Hash{}) {
		return fail("hash length is wrong")
	}
	if !bytes.Equal(row.PrevHash, v.prev[:]) {
		return fail("prev_hash does not match the previous row's hash")
	}
	var canonical []byte
	if v.canonical != nil {
		var err error
		if canonical, err = v.canonical(row); err != nil {
			return fail("%v", err)
		}
	} else {
		env, recanonical, err := recanonicalize(row.Event)
		if err != nil {
			return fail("%v", err)
		}
		if env.PartitionKey != row.PartitionKey || env.Sequence != FormatSequence(row.PartitionSeq) || env.ID != row.EventID {
			return fail("event envelope does not match its row")
		}
		canonical = recanonical
	}
	computed := ChainHash(v.prev, canonical)
	if !bytes.Equal(computed[:], row.Hash) {
		return fail("hash does not match the event content")
	}
	v.prev = computed
	v.next++
	v.checked++
	return nil
}
