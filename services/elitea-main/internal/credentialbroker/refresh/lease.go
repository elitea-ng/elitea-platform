// Package refresh runs a credential refresh once across every Main replica
// (issue #1121 item 3).
//
// PostgreSQL is the authority. A refresh needs the lease of its partition in
// elitea_runtime.credential_refresh_leases (shared migration 0190):
//
//   - Acquire is one INSERT ... ON CONFLICT DO UPDATE ... WHERE expires_at <
//     now(). It returns a new fence, or no row while another holder's lease
//     is live.
//   - Commit runs the token write-back in one transaction that first
//     releases the lease WHERE fence = $fence. A holder whose lease expired
//     and was taken over has a stale fence: its write-back is refused with
//     ErrStaleFence and nothing is written.
//
// Inside one process a singleflight group joins the callers of one
// partition, so a replica sends at most one acquire at a time. A caller that
// does not get the lease waits: it polls the state with bounded exponential
// backoff and full jitter until the holder's write-back is visible.
//
// See https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html
// for why the fence, not the lease expiry, protects the write.
package refresh

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PartitionHash is the SHA-256 of a refresh partition's identity.
type PartitionHash [32]byte

// Partition hashes the parts of a partition's identity under label. Parts
// are length-prefixed, so ("ab","c") and ("a","bc") differ.
func Partition(label string, parts ...string) PartitionHash {
	h := sha256.New()
	write := func(s string) {
		h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(s))))
		h.Write([]byte(s))
	}
	write(label)
	for _, p := range parts {
		write(p)
	}
	var out PartitionHash
	copy(out[:], h.Sum(nil))
	return out
}

// Lease is one acquired refresh lease.
type Lease struct {
	Partition PartitionHash
	Holder    string
	Fence     int64
	// Generation is the state generation the holder read when it acquired.
	Generation int64
}

// ErrStaleFence refuses a write-back whose lease was taken over.
var ErrStaleFence = errors.New("refresh: the lease fence is stale")

// Leases is the PostgreSQL lease table.
type Leases struct {
	pool *pgxpool.Pool
}

// NewLeases builds the lease store.
func NewLeases(pool *pgxpool.Pool) (*Leases, error) {
	if pool == nil {
		return nil, errors.New("refresh: a database pool is required")
	}
	return &Leases{pool: pool}, nil
}

const maxHolderBytes = 256

// Acquire takes the partition's lease for ttl when no live lease exists.
// acquired is false while another holder's lease is live.
func (l *Leases) Acquire(ctx context.Context, partition PartitionHash, holder string, ttl time.Duration, generation int64) (Lease, bool, error) {
	if holder == "" || len(holder) > maxHolderBytes || ttl <= 0 || generation < 0 {
		return Lease{}, false, errors.New("refresh: invalid lease request")
	}
	var fence int64
	err := l.pool.QueryRow(ctx, `
INSERT INTO elitea_runtime.credential_refresh_leases AS l
    (partition_hash, holder, fence, expires_at, generation)
VALUES ($1, $2, 1, clock_timestamp() + $3 * interval '1 microsecond', $4)
ON CONFLICT (partition_hash) DO UPDATE
    SET holder = EXCLUDED.holder,
        fence = l.fence + 1,
        expires_at = EXCLUDED.expires_at,
        generation = EXCLUDED.generation
    WHERE l.expires_at < clock_timestamp()
RETURNING fence`, partition[:], holder, ttl.Microseconds(), generation).Scan(&fence)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, fmt.Errorf("refresh: acquire the lease: %w", err)
	}
	return Lease{Partition: partition, Holder: holder, Fence: fence, Generation: generation}, true, nil
}

// Commit releases the lease and runs write in one transaction, only while
// the lease's fence is current. A stale fence is ErrStaleFence and write
// does not run. An error from write rolls the release back too.
func (l *Leases) Commit(ctx context.Context, lease Lease, write func(context.Context, pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		// The UPDATE locks the lease row until commit, so no acquirer can
		// take the lease between the fence check and the write.
		tag, err := tx.Exec(ctx, `
UPDATE elitea_runtime.credential_refresh_leases
   SET expires_at = clock_timestamp() - interval '1 microsecond', holder = ''
 WHERE partition_hash = $1 AND fence = $2`, lease.Partition[:], lease.Fence)
		if err != nil {
			return fmt.Errorf("refresh: check the lease fence: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleFence
		}
		return write(ctx, tx)
	})
}

// Release gives the lease up without a write, so a waiting replica can take
// it at once. A stale fence releases nothing.
func (l *Leases) Release(ctx context.Context, lease Lease) error {
	_, err := l.pool.Exec(ctx, `
UPDATE elitea_runtime.credential_refresh_leases
   SET expires_at = clock_timestamp() - interval '1 microsecond', holder = ''
 WHERE partition_hash = $1 AND fence = $2`, lease.Partition[:], lease.Fence)
	if err != nil {
		return fmt.Errorf("refresh: release the lease: %w", err)
	}
	return nil
}
