// stored_check.go implements the connection checks for an ALREADY SAVED
// credential: POST /check_stored_connection/[{mode}/]{projectID}/{configID}
// and POST /check_stored_connections/[{mode}/]{projectID}.
//
// # Why these exist beside check_connection.go
//
// The two routes in check_connection.go check a payload the CLIENT sends, so
// they can only test a credential whose api_key the client holds. Since the
// sealing change (secret_sealing.go) no read path returns that key any more:
// every stored `data` object carries a `{{secret.NAME}}` reference in its
// place. So the "test connection" control on a SAVED credential had no payload
// to send. Re-typing the key to re-test it is not a workaround — it is how a
// user discovers, by accident, that the platform is willing to test a key it
// was never given.
//
// THE DISCRIMINATING PROPERTY of this file is therefore: the check succeeds
// WITHOUT the client resending the secret. The row is read server-side and
// resolved through the SAME expander/unsecreter pair the admission decision
// uses (internal/runtimecomposition/configuration_provider_resolution.go owns
// that pair), so the reference redeems exactly as it does for the gateway.
// TestAStoredSealedCredentialChecksWithoutTheClientResendingTheSecret fails if
// a future edit starts reading the request body for the credential instead.
//
// # What this file must NOT do
//
//   - It must NOT persist anything. A provider round trip is not admission:
//     status_ok records that the platform ACCEPTED the row (its references
//     expand, its secrets redeem), and nothing in it contacts a provider — see
//     application/configurations/provider_admission.go, which says so at
//     length. Writing a provider verdict into that column would make the
//     gateway's admission gate mean two different things. Re-running admission
//     is a separate route, in revalidate.go, for exactly that reason.
//   - It must NOT dial the provider itself. The gateway owns the SSRF-safe
//     egress allowlist for a tenant-authored api_base (#13), and this handler
//     reaches it through the same GatewayConnectionChecker the unsaved check
//     uses.
//   - It must NOT log, return or otherwise expose the resolved plaintext. The
//     resolved payload goes to the checker and is dropped.
//
// A nil resolver or a nil checker REFUSES with the honest "not available"
// message the unsaved check already uses. It never fabricates success, and it
// never panics: every dependency is called through a nil test here and guards
// its own nil receiver on the other side, because a composition root that
// boxes a nil pointer into an interface makes the nil test false (the typed-nil
// lesson recorded on WithProviderAdmission).
package configurations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// errConfigurationStoreUnavailable reports that this router has no database to
// read the stored row from. It is a distinct value from a failed statement so
// a caller cannot confuse "not composed" with "the row is not there".
var errConfigurationStoreUnavailable = errors.New("configuration store is unavailable")

// configurationRowAbsent reports the ONE error that means "no such row".
// Every other error is a failure of the read, and reporting a failed read as
// an absent row is the recurring defect this repository keeps meeting:
// absence read as correctness.
func configurationRowAbsent(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// StoredConfigurationResolution names ONE stored row whose `data` column must
// be resolved to plaintext before a provider round trip.
//
// It carries the STORED data — the references, not the values. The resolver
// redeems them; this package never sees the vault.
type StoredConfigurationResolution struct {
	// ProjectID is the project that OWNS the row, which is the project whose
	// vault redeems its secrets. It is the {projectID} the schema was built
	// from, never a value out of the row's own JSON.
	ProjectID int32
	// AuthorID resolves a `private: true` reference against that user's
	// personal project, as the lifecycle resolution does. nil is valid: a row
	// with no author simply cannot resolve a private reference.
	AuthorID *int32
	// Data is the stored `data` object.
	Data map[string]any
}

// StoredConfigurationResolver expands a stored row's declared references and
// redeems its hidden secrets, returning the PLAINTEXT payload.
//
// It is the one capability that separates a stored check from the unsaved one.
// The implementation lives at the composition root
// (internal/runtimecomposition/configuration_stored_resolution.go) because the
// expander and the vault unsecreter it needs are owned by the Configurations
// runtime, and this package must not acquire a second vault of its own — a
// second vault with a second key source is #399's defect.
//
// Implementations MUST NOT persist, cache or log the resolved payload.
type StoredConfigurationResolver interface {
	ResolveStoredConfiguration(context.Context, StoredConfigurationResolution) (map[string]any, error)
}

// WithStoredConfigurationResolver wires the resolve+unseal capability the
// stored checks need.
//
// Without it BOTH stored routes report the honest "not available" failure for
// every row — never a fabricated success, and never a check of the stored
// `{{secret.NAME}}` reference as though it were the api_key, which would ask
// the provider to authenticate a literal template string and report a working
// credential as broken.
func WithStoredConfigurationResolver(resolver StoredConfigurationResolver) Option {
	return func(handler *Handler) {
		handler.storedResolver = resolver
	}
}

// storedConnectionCheckUnavailableMessage is the message a missing dependency
// answers with. It is the string the unsaved check already uses for the same
// condition, so the browser's toast does not change wording depending on which
// of the two routes it called.
const storedConnectionCheckUnavailableMessage = "Connection checking is not available right now."

// storedConfigurationRow is the part of a configuration row a stored check
// reads. It deliberately holds no status column: this file writes none.
type storedConfigurationRow struct {
	id         int
	uuid       string
	configType string
	data       map[string]any
	authorID   *int
}

// CheckStoredConnection validates a SAVED credential against the real
// provider, reading the credential from the row rather than from the request.
//
// The response contract is the unsaved check's, byte for byte, because the
// same browser control renders both: HTTP 200 with {"success":true} only on a
// proven round trip, and HTTP 400 with {"success":false,"message":...} for
// every failure. A row this project does not own is a 404 — the schema is
// built from the {projectID} in the path, so a configID belonging to another
// project simply is not in the table this statement reads.
func (h *Handler) CheckStoredConnection(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	configID := chi.URLParam(r, "configID")
	schema, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()

	// The request body is not read at all, and that is the point of the route:
	// there is nothing a client could send that this check needs.
	row, found, err := h.loadStoredConfigurationRow(ctx, schema, configID)
	if err != nil {
		// A failed READ is not a missing row and not a provider verdict. It
		// answers "could not verify", never 404: reporting a saturated pool as
		// a deleted credential is how a user is told to re-create a credential
		// that is still there.
		slog.ErrorContext(ctx, "check_stored_connection: read the configuration row failed",
			"project_id", projectID, "configuration_id", configID, "err", err)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "Could not verify the connection right now. Please try again.",
		})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"success": false,
			"message": "Configuration not found",
		})
		return
	}

	result, status := h.checkStoredRow(ctx, projectID, row)
	writeJSON(w, status, storedConnectionCheckBody(result))
}

// storedConnectionCheckBody is the one response shape both stored routes write.
// `reason` is present only for a toolkit probe, which is the one path with a
// closed vocabulary to publish; the LLM path keeps the {success,message} body
// it always answered, so no existing reader changes.
func storedConnectionCheckBody(result ConnectionCheckResult) map[string]any {
	body := map[string]any{"success": result.Success, "message": result.Message}
	if result.Reason != "" {
		body["reason"] = result.Reason
	}
	return body
}

// storedConnectionCheckBatchRequest is the batch body. The web app sends the
// ids of the rows currently on screen, so the field is a list of the same
// `{configID}` values the single route takes: an integer id or a uuid.
type storedConnectionCheckBatchRequest struct {
	ConfigurationIDs []any `json:"configuration_ids"`
}

// BatchCheckStoredConnections checks many saved credentials in one request.
//
// The contract mirrors BatchCheckConnections: ALWAYS HTTP 200, one result
// object per input item, in input order. The web app marks every credential
// invalid when this request fails, so a 4xx would paint a healthy project red.
func (h *Handler) BatchCheckStoredConnections(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	schema, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}

	var body storedConnectionCheckBatchRequest
	if !decodeBoundedJSON(w, r, &body) {
		return
	}

	// The same three bounds the unsaved batch applies, and the same constants:
	// an item cap, one deadline for the whole request, and a worker pool so a
	// legitimate list finishes inside that deadline.
	ctx, cancel := context.WithTimeout(r.Context(), batchConnectionCheckBudget)
	defer cancel()

	items := body.ConfigurationIDs
	results := make([]map[string]any, len(items))
	checked := items
	if len(checked) > maxBatchConnectionChecks {
		checked = items[:maxBatchConnectionChecks]
		slog.WarnContext(ctx, "check_stored_connections: item count above the cap",
			"project_id", projectID, "items", len(items), "cap", maxBatchConnectionChecks)
		for index := maxBatchConnectionChecks; index < len(items); index++ {
			results[index] = connectionCheckUnavailableResult(items[index])
		}
	}

	// One statement reads every requested row, rather than one statement per
	// item: the per-item form turns a 200-row page into 200 round trips
	// competing with the six provider calls in flight for the same pool.
	rows, err := h.loadStoredConfigurationRows(ctx, schema, checked)
	if err != nil {
		slog.ErrorContext(ctx, "check_stored_connections: read the configuration rows failed",
			"project_id", projectID, "err", err)
		for index := range checked {
			results[index] = connectionCheckUnavailableResult(items[index])
		}
		writeJSON(w, http.StatusOK, results)
		return
	}

	positions := make(chan int)
	var workers sync.WaitGroup
	for worker := 0; worker < batchConnectionCheckWorkers; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			// Each worker writes its own index, so the slice needs no lock and
			// the input order survives.
			for index := range positions {
				results[index] = h.checkStoredBatchItem(ctx, projectID, checked[index], rows)
			}
		}()
	}
	for index := range checked {
		positions <- index
	}
	close(positions)
	workers.Wait()

	writeJSON(w, http.StatusOK, results)
}

// checkStoredBatchItem produces the one result object for one requested id.
func (h *Handler) checkStoredBatchItem(
	ctx context.Context,
	projectID string,
	requested any,
	rows map[string]storedConfigurationRow,
) map[string]any {
	key, ok := storedConfigurationRowKey(requested)
	if !ok {
		return map[string]any{"id": requested, "success": false, "message": "Configuration not found"}
	}
	row, found := rows[key]
	if !found {
		// A row this project does not hold is the batch form of the single
		// route's 404. It is not "unsupported": the type is not the reason.
		return map[string]any{"id": requested, "success": false, "message": "Configuration not found"}
	}
	if _, known := h.catalog.EntryByType(row.configType); !known {
		// Same distinction the unsaved batch draws, and it is answered BEFORE
		// the row is checked: `unsupported` marks a type this build has never
		// heard of, NOT a known type with no working check yet (which carries
		// the not-supported message instead).
		return map[string]any{"id": requested, "success": false, "unsupported": true}
	}
	// Read the deadline here rather than rely on Check returning an error, so
	// an expired budget costs no syscall for each remaining item.
	if ctx.Err() != nil {
		return connectionCheckUnavailableResult(requested)
	}
	result, _ := h.checkStoredRow(ctx, projectID, row)
	item := storedConnectionCheckBody(result)
	item["id"] = requested
	return item
}

// checkStoredRow is the whole decision for one stored row: what may be
// checked, what resolves, and what the provider answered. It returns the
// result and the HTTP status the SINGLE route reports (the batch route always
// answers 200 and reads the result only).
//
// It writes nothing, in any branch.
func (h *Handler) checkStoredRow(
	ctx context.Context,
	projectID string,
	row storedConfigurationRow,
) (ConnectionCheckResult, int) {
	if _, known := h.catalog.EntryByType(row.configType); !known {
		return ConnectionCheckResult{
			Message: fmt.Sprintf("Unknown configuration type: %s", row.configType),
		}, http.StatusNotFound
	}
	if _, checkable := checkableConnectionTypes[row.configType]; !checkable {
		// A TOOLKIT credential takes this build's own probe. It still resolves
		// first, through the SAME resolver the LLM path uses, because the row
		// holds a `{{secret.NAME}}` reference and not a token: probing the
		// reference would ask GitHub to authenticate a template string and
		// report every saved credential as refused.
		if IsToolkitCheckableType(row.configType) {
			return h.checkStoredToolkitRow(ctx, projectID, row)
		}
		// The same byte-for-byte message the unsaved check answers, from the
		// same function: a stored row and an unsaved payload of the same type
		// must not disagree about whether the platform can check it.
		return ConnectionCheckResult{
			Message: connectionCheckNotSupportedMessage(row.configType),
			Reason:  ToolkitCheckReasonUnsupportedType,
		}, http.StatusBadRequest
	}
	if h.storedResolver == nil || h.connectionChecker == nil {
		slog.ErrorContext(ctx, "check_stored_connection: the stored check is not composed",
			"type", row.configType, "project_id", projectID,
			"resolver", h.storedResolver != nil, "checker", h.connectionChecker != nil)
		return ConnectionCheckResult{Message: storedConnectionCheckUnavailableMessage}, http.StatusBadRequest
	}

	// tenantSchema already refused a non-decimal id, so a refusal here is the
	// out-of-range case only. A truncated project id would redeem another
	// project's vault, so it is refused rather than narrowed.
	resolution, ok := storedResolutionFor(projectID, row)
	if !ok {
		return ConnectionCheckResult{Message: storedConnectionCheckUnavailableMessage}, http.StatusBadRequest
	}

	resolved, err := h.storedResolver.ResolveStoredConfiguration(ctx, resolution)
	if err != nil || resolved == nil {
		// A row whose references do not expand, or whose secret the vault
		// cannot redeem, is a real failure of THIS credential — it is exactly
		// the row the gateway will refuse — but it is not a provider verdict,
		// and the cause never reaches the browser.
		slog.WarnContext(ctx, "check_stored_connection: the stored configuration did not resolve",
			"type", row.configType, "project_id", projectID, "configuration_id", row.id, "err", err)
		return ConnectionCheckResult{
			Message: "This credential could not be resolved. Check that its secret and any referenced configuration still exist.",
		}, http.StatusBadRequest
	}
	// The guard runs on the RESOLVED payload, because expansion can merge a
	// referenced row's api_base into it: the value that reaches the provider
	// is the value the guard has to see.
	if err := validateNotSelfReferential(resolved, selfLLMOrigins()); err != nil {
		return ConnectionCheckResult{Message: err.Error()}, http.StatusBadRequest
	}

	result, err := h.connectionChecker.Check(
		WithConnectionCheckProjectID(ctx, projectID), row.configType, resolved)
	if err != nil {
		// A transport-level failure must never be reported as success, and the
		// real cause is logged server-side only.
		slog.ErrorContext(ctx, "check_stored_connection: checker call failed",
			"type", row.configType, "project_id", projectID, "configuration_id", row.id, "err", err)
		return ConnectionCheckResult{
			Message: "Could not verify the connection right now. Please try again.",
		}, http.StatusBadRequest
	}
	if result.Success {
		return result, http.StatusOK
	}
	return result, http.StatusBadRequest
}

// loadStoredConfigurationRow reads one row of the project's own schema.
//
// `found` is false for a row that is not there, which is what a configID from
// ANOTHER project looks like from here: the schema in the statement is the one
// {projectID} named, so a cross-project id names no row and answers 404.
func (h *Handler) loadStoredConfigurationRow(
	ctx context.Context,
	schema string,
	configID string,
) (storedConfigurationRow, bool, error) {
	if h.pool == nil {
		return storedConfigurationRow{}, false, errConfigurationStoreUnavailable
	}
	query := fmt.Sprintf(
		`SELECT id, COALESCE(uuid::text, ''), type, data, author_id
		 FROM %s.configuration WHERE %s = $1`,
		schema, configurationIDColumn(configID))

	var row storedConfigurationRow
	var data []byte
	if err := h.pool.QueryRow(ctx, query, configID).Scan(
		&row.id, &row.uuid, &row.configType, &data, &row.authorID,
	); err != nil {
		// A missing row and a failed statement are NOT the same answer. The
		// first is a 404; the second must not read as one, or a saturated pool
		// reports every credential as deleted.
		if configurationRowAbsent(err) {
			return storedConfigurationRow{}, false, nil
		}
		return storedConfigurationRow{}, false, err
	}
	if err := json.Unmarshal(data, &row.data); err != nil {
		return storedConfigurationRow{}, false, err
	}
	if row.data == nil {
		row.data = map[string]any{}
	}
	return row, true, nil
}

// loadStoredConfigurationRows reads every requested row in one statement, and
// keys them by BOTH forms a caller may address them with.
func (h *Handler) loadStoredConfigurationRows(
	ctx context.Context,
	schema string,
	requested []any,
) (map[string]storedConfigurationRow, error) {
	rows := make(map[string]storedConfigurationRow, len(requested))
	if len(requested) == 0 {
		return rows, nil
	}
	if h.pool == nil {
		return nil, errConfigurationStoreUnavailable
	}

	numeric := make([]int32, 0, len(requested))
	textual := make([]string, 0, len(requested))
	for _, item := range requested {
		key, ok := storedConfigurationRowKey(item)
		if !ok {
			continue
		}
		if id, err := strconv.Atoi(key); err == nil {
			if id <= 0 || id > math.MaxInt32 {
				continue
			}
			numeric = append(numeric, int32(id))
			continue
		}
		textual = append(textual, key)
	}
	if len(numeric) == 0 && len(textual) == 0 {
		return rows, nil
	}

	// Both predicates compare TEXT or INTEGER against a bound array, so no
	// caller value reaches the statement text — only the schema name does, and
	// tenantSchema quoted it with SQL rules (#543).
	query := fmt.Sprintf(
		`SELECT id, COALESCE(uuid::text, ''), type, data, author_id
		 FROM %s.configuration
		 WHERE id = ANY($1::integer[]) OR uuid::text = ANY($2::text[])
		 LIMIT $3`,
		schema)
	result, err := h.pool.Query(ctx, query, numeric, textual, maxBatchConnectionChecks)
	if err != nil {
		return nil, err
	}
	defer result.Close()

	for result.Next() {
		var row storedConfigurationRow
		var data []byte
		if err := result.Scan(&row.id, &row.uuid, &row.configType, &data, &row.authorID); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &row.data); err != nil {
			return nil, err
		}
		if row.data == nil {
			row.data = map[string]any{}
		}
		rows[strconv.Itoa(row.id)] = row
		if row.uuid != "" {
			rows[row.uuid] = row
		}
	}
	// rows.Err() decides whether this is a complete answer. Without it a result
	// set that failed part way through reads as "those rows do not exist", and
	// every unread credential is reported as deleted.
	if err := result.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}

// storedConfigurationRowKey renders one requested id as the string form the
// row map is keyed by.
//
// The JSON body may carry either form the URL takes: an integer id or a uuid
// string. A JSON number decodes as float64, so an integral one is rendered
// without its ".0"; anything else — a fraction, a bool, an object — names no
// row and is reported as not found rather than silently skipped.
func storedConfigurationRowKey(requested any) (string, bool) {
	switch value := requested.(type) {
	case string:
		if value == "" || len(value) > maxConfigurationFilterLength {
			return "", false
		}
		return value, true
	case float64:
		if value <= 0 || value > math.MaxInt32 || math.Trunc(value) != value {
			return "", false
		}
		return strconv.Itoa(int(value)), true
	default:
		return "", false
	}
}

// checkStoredToolkitRow resolves a saved TOOLKIT credential and probes it.
//
// It is the stored half of toolkit_check.go, and it shares every rule the LLM
// path above obeys: it writes nothing, it never reports success without a round
// trip, and the resolved plaintext is handed to the checker and dropped.
func (h *Handler) checkStoredToolkitRow(
	ctx context.Context,
	projectID string,
	row storedConfigurationRow,
) (ConnectionCheckResult, int) {
	if h.storedResolver == nil || h.toolkitChecker == nil {
		slog.ErrorContext(ctx, "check_stored_connection: the toolkit check is not composed",
			"type", row.configType, "project_id", projectID,
			"resolver", h.storedResolver != nil, "checker", h.toolkitChecker != nil)
		return ConnectionCheckResult{Message: storedConnectionCheckUnavailableMessage}, http.StatusBadRequest
	}

	resolution, ok := storedResolutionFor(projectID, row)
	if !ok {
		return ConnectionCheckResult{Message: storedConnectionCheckUnavailableMessage}, http.StatusBadRequest
	}
	resolved, err := h.storedResolver.ResolveStoredConfiguration(ctx, resolution)
	if err != nil || resolved == nil {
		slog.WarnContext(ctx, "check_stored_connection: the stored toolkit configuration did not resolve",
			"type", row.configType, "project_id", projectID, "configuration_id", row.id, "err", err)
		return ConnectionCheckResult{
			Message: "This credential could not be resolved. Check that its secret and any referenced configuration still exist.",
		}, http.StatusBadRequest
	}

	outcome := h.toolkitChecker.CheckToolkit(ctx, row.configType, resolved)
	result := ConnectionCheckResult{Success: outcome.Success(), Message: outcome.Message, Reason: outcome.Reason}
	return result, toolkitCheckStatus(outcome)
}
