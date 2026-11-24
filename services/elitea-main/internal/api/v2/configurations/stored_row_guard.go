package configurations

// The stored-row guard of a global provider update (ADR-0059 R1.2b, issue
// 2993). The global provider route classifies an update against the stored
// row it read before the write (rewriteGlobalProviderBody), and merges the
// stored `data` under the body. For a personal access token that the token
// rule refuses the `route` outcome, that read is the whole decision: if the
// row changed after it, the merged `data` would write the endpoint or the
// credential it read back over the newer one, a route change the token may
// not make. The guard holds the write to the row the classification saw: the
// update's transaction reads the row under its lock and refuses the write
// with 409 when it is not the same.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"

	"github.com/jackc/pgx/v5"
)

// storedRowGuard is the stored row an update was classified against.
type storedRowGuard struct {
	configType string
	data       map[string]any
}

type storedRowGuardKey struct{}

// withStoredRowGuard holds the next update of the request to guard.
func withStoredRowGuard(ctx context.Context, guard storedRowGuard) context.Context {
	return context.WithValue(ctx, storedRowGuardKey{}, guard)
}

// checkStoredRowGuard refuses the update with 409 when ctx holds a guard and
// the stored row, read under its lock in tx, is not the row the guard names.
// No guard, no read.
func checkStoredRowGuard(
	ctx context.Context, tx pgx.Tx, schema, configID string,
) (*configurationWriteFailure, error) {
	guard, ok := ctx.Value(storedRowGuardKey{}).(storedRowGuard)
	if !ok {
		return nil, nil
	}
	var raw []byte
	var configType string
	if err := tx.QueryRow(ctx, fmt.Sprintf(
		`SELECT data, type FROM %s.configuration WHERE %s = $1 FOR UPDATE`,
		schema, configurationIDColumn(configID)), configID).Scan(&raw, &configType); err != nil {
		return nil, err
	}
	var data map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &data); err != nil {
			return invalidStoredConfiguration(), nil
		}
	}
	if configType == guard.configType && sameData(data, guard.data) {
		return nil, nil
	}
	return &configurationWriteFailure{
		status:  http.StatusConflict,
		message: "the provider changed while this update was made: read it again and retry",
	}, nil
}

// sameData compares two decoded `data` objects; nil and empty are the same.
func sameData(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}
