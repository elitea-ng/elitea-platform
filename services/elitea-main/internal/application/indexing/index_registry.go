package indexing

import (
	"context"
	"errors"
	"math"
	"time"
)

// This file is the Rust-runtime counterpart of index_meta.go. Under
// ELITEA_INDEXING_RUNTIME=rust (ADR-0030 decision 6) index metadata lives in
// elitea_runtime.index_registry instead of an `index_meta` row in the project's
// pgvector database, and elitea-main is the only writer of it (ADR-0030
// decision 2). So:
//
//   - the admission initializer writes the registry row, and needs neither a
//     frozen toolkit configuration nor a pgvector connection string;
//   - the terminal and manual-Stop effects fence on the same admission binding
//     the Python path uses, and write the registry row;
//   - a successful result is applied from the typed IndexIngestSummaryV1 in
//     the output projection transaction (infra/db/repos), not here.
//
// The interfaces below are the seams. The Postgres repository implements
// them; nothing in this file knows about SQL.

// RegistryInitialRun is the exact first write of one admitted run.
type RegistryInitialRun struct {
	ProjectID       int32
	ToolkitID       int32
	IndexName       string
	MetaID          string
	ExecutionID     string
	CorrelationID   string
	Generation      uint64
	IndexGeneration uint64
	// Configuration is the `index_data` arguments, a JSON object, stored as the
	// index_configuration the next reindex replays.
	Configuration []byte
	AdmittedAt    time.Time
}

func (r RegistryInitialRun) Validate() error {
	if r.ProjectID <= 0 || r.ToolkitID <= 0 ||
		r.IndexName == "" || len(r.IndexName) > maxIndexAdmissionStringBytes ||
		!validOptionalText(r.MetaID, maxIndexAdmissionStringBytes) || r.MetaID == "" ||
		!validOptionalText(r.ExecutionID, maxIndexAdmissionStringBytes) || r.ExecutionID == "" ||
		!validOptionalText(r.CorrelationID, maxIndexAdmissionStringBytes) || r.CorrelationID == "" ||
		r.Generation == 0 || r.Generation > math.MaxInt64 ||
		r.IndexGeneration == 0 || r.IndexGeneration > math.MaxInt64 ||
		len(r.Configuration) == 0 || len(r.Configuration) > MaxCurrentInitialIndexMetaBytes ||
		!validBoundedJSONObject(r.Configuration) || r.AdmittedAt.IsZero() {
		return ErrCurrentIndexMetaInitializationInvalid
	}
	return nil
}

// RegistryRunWriter creates the registry row for an admitted run, or starts
// the next run on an existing one. It must be idempotent for the same
// (ExecutionID, Generation), return ErrCurrentIndexMetaConflict when another
// run is still active, and ErrCurrentIndexMetaSuperseded when a newer index
// generation already holds the row.
type RegistryRunWriter interface {
	InitializeRegistryRun(context.Context, RegistryInitialRun) error
}

// RegistryIndexMetaInitializer is the rust-mode IndexMetaMaterializer.
type RegistryIndexMetaInitializer struct {
	writer RegistryRunWriter
}

func NewRegistryIndexMetaInitializer(writer RegistryRunWriter) (*RegistryIndexMetaInitializer, error) {
	if writer == nil {
		return nil, errors.New("index registry initializer dependencies are required")
	}
	return &RegistryIndexMetaInitializer{writer: writer}, nil
}

func (i *RegistryIndexMetaInitializer) MaterializeInitialIndexMeta(
	ctx context.Context,
	request SubmitRequest,
	outcome AdmissionOutcome,
) error {
	if ctx == nil || i == nil || i.writer == nil {
		return ErrCurrentIndexMetaInitializationInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// The same identity checks CurrentIndexMetaInitializer applies: the row is
	// written for the project the admission was made in, by the admitting
	// actor's toolkit, and no other.
	if request.ToolkitID <= 0 || request.Inputs.validate() != nil ||
		request.Identity.TenantID != request.Identity.ResourceProjectID ||
		request.Identity.ProjectionProjectID != request.Identity.ResourceProjectID ||
		outcome.ExecutionID == "" || outcome.Generation == 0 ||
		outcome.IndexGeneration == 0 || outcome.IndexGeneration > math.MaxInt64 ||
		outcome.IndexMetaID == "" || outcome.IndexMetaCorrelationID == "" ||
		outcome.IndexMetaCorrelationID != request.CorrelationID ||
		outcome.AdmittedAt.IsZero() {
		return ErrCurrentIndexMetaInitializationInvalid
	}
	projectID, ok := currentIndexMetaIdentityID(request.Identity.ResourceProjectID)
	if !ok {
		return ErrCurrentIndexMetaInitializationInvalid
	}
	indexName, err := indexNameFromToolParameters(request.Inputs.ToolParameters)
	if err != nil {
		return ErrCurrentIndexMetaInitializationInvalid
	}
	// The parameters are stored as the configuration; decode only to prove they
	// are one bounded JSON object.
	if _, err := decodeCurrentIndexMetaObject(request.Inputs.ToolParameters); err != nil {
		return ErrCurrentIndexMetaInitializationInvalid
	}
	run := RegistryInitialRun{
		ProjectID:       projectID,
		ToolkitID:       request.ToolkitID,
		IndexName:       indexName,
		MetaID:          outcome.IndexMetaID,
		ExecutionID:     outcome.ExecutionID,
		CorrelationID:   outcome.IndexMetaCorrelationID,
		Generation:      outcome.Generation,
		IndexGeneration: outcome.IndexGeneration,
		Configuration:   append([]byte(nil), request.Inputs.ToolParameters...),
		AdmittedAt:      outcome.AdmittedAt.UTC(),
	}
	if err := run.Validate(); err != nil {
		return err
	}
	if err := i.writer.InitializeRegistryRun(ctx, run); err != nil {
		return currentIndexMetaInitializationError(ctx, err)
	}
	return nil
}

var _ IndexMetaMaterializer = (*RegistryIndexMetaInitializer)(nil)

// RegistryTerminal is a failed or cancelled transition of one run.
type RegistryTerminal struct {
	ProjectID int32
	CurrentTerminalIndexMeta
}

func (t RegistryTerminal) Validate() error {
	if t.ProjectID <= 0 {
		return ErrCurrentIndexMetaInitializationInvalid
	}
	return t.CurrentTerminalIndexMeta.Validate()
}

// RegistryTerminalWriter applies a failed or cancelled transition to the
// registry row of the run it names. It fences on (ExecutionID, Generation,
// IndexGeneration): a transition for an older run returns
// ErrCurrentIndexMetaSuperseded and one that names no run returns
// ErrCurrentIndexMetaConflict. A row already terminal for the same run is a
// no-op: the first terminal transition wins.
type RegistryTerminalWriter interface {
	ApplyRegistryTerminal(context.Context, RegistryTerminal) error
}

// RegistryIndexMetaTerminalizer is the rust-mode CurrentIndexMetaTerminalizer.
// The Python one redeems the frozen toolkit configuration to find the pgvector
// target; a registry row is addressed by the admission binding alone, so this
// one does not.
type RegistryIndexMetaTerminalizer struct {
	bindings CurrentIndexMetaTerminalBindingRepository
	writer   RegistryTerminalWriter
}

func NewRegistryIndexMetaTerminalizer(
	bindings CurrentIndexMetaTerminalBindingRepository,
	writer RegistryTerminalWriter,
) (*RegistryIndexMetaTerminalizer, error) {
	if bindings == nil || writer == nil {
		return nil, errors.New("index registry terminalizer dependencies are required")
	}
	return &RegistryIndexMetaTerminalizer{bindings: bindings, writer: writer}, nil
}

func (t *RegistryIndexMetaTerminalizer) Terminalize(
	ctx context.Context,
	request CurrentIndexMetaTerminalRequest,
) error {
	if t == nil || t.bindings == nil || t.writer == nil || ctx == nil {
		return ErrCurrentIndexMetaInitializationInvalid
	}
	if err := request.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	binding, err := t.bindings.LoadCurrentIndexMetaTerminalBinding(ctx, request.ExecutionID, request.Generation)
	if err != nil {
		return currentIndexMetaInitializationError(ctx, err)
	}
	if err := binding.Validate(); err != nil ||
		binding.ExecutionID != request.ExecutionID ||
		binding.Generation != request.Generation {
		return ErrCurrentIndexMetaConflict
	}
	record := RegistryTerminal{
		ProjectID: binding.ResourceProjectID,
		CurrentTerminalIndexMeta: CurrentTerminalIndexMeta{
			MetaID:          binding.MetaID,
			ExecutionID:     binding.ExecutionID,
			Generation:      binding.Generation,
			IndexGeneration: binding.IndexGeneration,
			IndexName:       binding.IndexName,
			ToolkitID:       binding.ToolkitID,
			State:           request.State,
			OccurredAt:      request.OccurredAt.UTC(),
			SafeError:       request.SafeError,
		},
	}
	if err := record.Validate(); err != nil {
		return err
	}
	if err := t.writer.ApplyRegistryTerminal(ctx, record); err != nil {
		return currentIndexMetaInitializationError(ctx, err)
	}
	return nil
}

// IndexVectorNamespace names the points of one index in the vector store:
// ADR-0031 decision 2's `namespace_id` is the index_registry UUID, and the
// project is the tenant.
type IndexVectorNamespace struct {
	ProjectID int32
	IndexID   string
}

// ErrIndexVectorDeletionDeferred is what a VectorDeleter without a vector
// store client returns. It is not a failure: it tells the caller that the
// deletion has NOT happened and must be repeated, so the registry keeps its
// tombstone instead of forgetting vectors it never deleted.
var ErrIndexVectorDeletionDeferred = errors.New("index vector deletion is deferred until elitea-vector is wired")

// IndexVectorDeleter is the hook to elitea-vector's Delete (ADR-0031 decision
// 4: "a toolkit index: deleting it calls Delete on its namespace").
//
// It is called when an index is deleted and again, by the tombstone sweeper,
// for every tombstone it still holds. It is NOT called when a run is stopped:
// a stop keeps the index's vectors (see RegistryManualStopCleaner).
//
// TODO(ADR-0031 V2): elitea-main has no elitea-vector client yet. Until it
// does, the composition root installs DeferredIndexVectorDeleter, which keeps
// every deletion pending and makes the sweeper idle. The implementation must be
// idempotent: the sweeper calls it again for a tombstone it still holds.
type IndexVectorDeleter interface {
	DeleteIndexVectors(context.Context, IndexVectorNamespace) error
}

// DeferredIndexVectorDeleter is the installed hook while no vector store client
// exists. It never reports success.
type DeferredIndexVectorDeleter struct{}

func (DeferredIndexVectorDeleter) DeleteIndexVectors(context.Context, IndexVectorNamespace) error {
	return ErrIndexVectorDeletionDeferred
}

// RegistryManualStop is the immutable evidence of a manual Stop.
type RegistryManualStop struct {
	ProjectID int32
	CurrentManualStopCleanup
}

func (s RegistryManualStop) Validate() error {
	if s.ProjectID <= 0 {
		return ErrCurrentIndexMetaInitializationInvalid
	}
	return s.CurrentManualStopCleanup.Validate()
}

// RegistryManualStopWriter checks that the registry row of the stopped run is
// cancelled for exactly that run. It writes nothing: the cancelled transition
// itself is RegistryTerminalWriter's. An empty indexID with a nil error means
// no row names the run (it was admitted under the python runtime, before the
// mode switched), and there is nothing to clean.
type RegistryManualStopWriter interface {
	VerifyRegistryManualStop(context.Context, RegistryManualStop) (indexID string, err error)
}

// RegistryManualStopCleaner is the rust-mode counterpart of
// CurrentManualStopCleaner, and it deletes nothing.
//
// The Python cleaner deletes the stopped run's partially written embeddings
// from the project's pgvector table, because each embedding row is tagged with
// the run that wrote it. The registry has no such tag: its vectors belong to
// the INDEX namespace, and a reindex is incremental (ADR-0030 decision 3). A
// stopped reindex of a completed index would, if its namespace were deleted,
// destroy every vector the index held before the run started and leave it
// "cancelled" and empty. So a manual stop only verifies that the registry row
// is cancelled for exactly the stopped run; the vectors and the
// index_registry_documents rows stay as the run wrote them, and the next run
// reconciles from the documents table: a document whose recorded version is
// unchanged is skipped, a changed one is rewritten by deleting its chunks by
// key, a vanished one is forgotten. Only deleting the whole index removes its
// vectors (the tombstone sweeper).
type RegistryManualStopCleaner struct {
	bindings CurrentIndexMetaTerminalBindingRepository
	registry RegistryManualStopWriter
}

func NewRegistryManualStopCleaner(
	bindings CurrentIndexMetaTerminalBindingRepository,
	registry RegistryManualStopWriter,
) (*RegistryManualStopCleaner, error) {
	if bindings == nil || registry == nil {
		return nil, errors.New("index registry manual Stop cleanup dependencies are required")
	}
	return &RegistryManualStopCleaner{bindings: bindings, registry: registry}, nil
}

func (c *RegistryManualStopCleaner) Cleanup(
	ctx context.Context,
	request CurrentManualStopCleanupRequest,
) error {
	if c == nil || c.bindings == nil || c.registry == nil || ctx == nil {
		return ErrCurrentIndexMetaInitializationInvalid
	}
	if err := request.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	binding, err := c.bindings.LoadCurrentIndexMetaTerminalBinding(ctx, request.ExecutionID, request.Generation)
	if err != nil {
		return currentIndexMetaInitializationError(ctx, err)
	}
	if err := binding.Validate(); err != nil ||
		binding.ExecutionID != request.ExecutionID ||
		binding.Generation != request.Generation {
		return ErrCurrentIndexMetaConflict
	}
	stop := RegistryManualStop{
		ProjectID: binding.ResourceProjectID,
		CurrentManualStopCleanup: CurrentManualStopCleanup{
			MetaID:          binding.MetaID,
			ExecutionID:     binding.ExecutionID,
			Generation:      binding.Generation,
			IndexGeneration: binding.IndexGeneration,
			IndexName:       binding.IndexName,
			ToolkitID:       binding.ToolkitID,
		},
	}
	if err := stop.Validate(); err != nil {
		return err
	}
	if _, err := c.registry.VerifyRegistryManualStop(ctx, stop); err != nil {
		return currentIndexMetaInitializationError(ctx, err)
	}
	return nil
}
