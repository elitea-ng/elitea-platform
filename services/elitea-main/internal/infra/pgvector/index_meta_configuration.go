package pgvector

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	indexmetaapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexmeta"
	"github.com/jackc/pgx/v5"
)

// CurrentIndexConfigurationWriter replaces ONE field of ONE index metadata
// document: `index_configuration`.
//
// Why a separate writer rather than a method on CurrentIndexMetaWriter: every
// method there plans a LIFECYCLE transition (an initial generation, a terminal
// state, a task-id restamp) and validates a run's admission evidence —
// generation fences, execution ids, created markers. This write belongs to no
// run. It carries no generation, it must not advance one, and it must leave
// `state`, `task_id`, `history` and the document counters exactly as it found
// them, because the index the user is configuring is a finished index whose
// data this save deliberately does not touch.
//
// The value is stored as a JSON STRING, not as a nested object. That is the
// form the Python indexer writes and the form the read path expects:
// `indexmeta.decodeCurrentNestedJSON` decodes a stored string into the object
// the UI reads, and `indexschedule.scheduledIndexParameters` accepts the
// string directly as the next run's tool parameters. Storing a bare object
// here would still be READ correctly but would diverge from every row a run
// has ever written, for no gain.
type CurrentIndexConfigurationWriter struct {
	queryTimeout time.Duration
	gate         chan struct{}
}

func NewCurrentIndexConfigurationWriter() *CurrentIndexConfigurationWriter {
	return &CurrentIndexConfigurationWriter{
		queryTimeout: defaultCurrentIndexMetaQueryTimeout,
		gate:         make(chan struct{}, defaultCurrentIndexMetaConnections),
	}
}

// SaveConfiguration commits the replacement, or reports that the index does
// not exist. It never creates the document: a configuration saved against an
// index that was never created would be invisible to every reader and would
// resurrect as a phantom row the moment the collection name was reused.
func (w *CurrentIndexConfigurationWriter) SaveConfiguration(
	ctx context.Context,
	target indexmetaapp.ResolvedTarget,
	indexName string,
	configuration json.RawMessage,
) error {
	if w == nil || ctx == nil || w.queryTimeout <= 0 || w.gate == nil ||
		cap(w.gate) <= 0 || target.SchemaID <= 0 ||
		target.ConnectionString == "" ||
		len(target.ConnectionString) > indexmetaapp.MaxCurrentPgvectorDSNBytes ||
		strings.ContainsAny(target.ConnectionString, "\x00\r\n") ||
		!validCurrentIndexMetaCollection(indexName) ||
		len(configuration) == 0 ||
		len(configuration) > indexmetaapp.MaxCurrentIndexConfigurationBytes ||
		!json.Valid(configuration) {
		return ErrCurrentIndexMetaWrite
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case w.gate <- struct{}{}:
		defer func() { <-w.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}

	dsn, ok := normalizeCurrentPgvectorDSN(target.ConnectionString)
	if !ok {
		return ErrCurrentIndexMetaWrite
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return ErrCurrentIndexMetaWrite
	}
	queryContext, cancel := context.WithTimeout(ctx, w.queryTimeout)
	defer cancel()
	connection, err := pgx.ConnectConfig(queryContext, config)
	if err != nil {
		return currentIndexMetaWriteError(queryContext, err)
	}
	defer closeCurrentIndexMetaDeleteConnection(connection)

	// The stored form: a JSON string whose contents are the configuration
	// object. `json.Marshal` of a string is what produces the quoted,
	// escaped literal jsonb needs.
	encoded, err := json.Marshal(string(configuration))
	if err != nil {
		return ErrCurrentIndexMetaWrite
	}

	// jsonb_set on exactly one key. The whole document is never rewritten, so
	// a concurrent run's state/history write cannot be clobbered by this save
	// and no row lock is needed for correctness beyond the statement's own.
	tag, err := connection.Exec(queryContext, `
UPDATE `+pgx.Identifier{strconv.FormatInt(int64(target.SchemaID), 10)}.Sanitize()+`.langchain_pg_embedding
SET cmetadata = jsonb_set(cmetadata, '{index_configuration}', $2::jsonb, true)
WHERE cmetadata @> '{"type":"index_meta"}'::jsonb
  AND cmetadata->>'collection' = $1`,
		indexName, string(encoded),
	)
	if err != nil {
		return currentIndexMetaWriteError(queryContext, err)
	}
	if tag.RowsAffected() == 0 {
		return indexmetaapp.ErrCurrentIndexMetaNotFound
	}
	return nil
}

var _ indexmetaapp.ExternalConfigurationWriter = (*CurrentIndexConfigurationWriter)(nil)
