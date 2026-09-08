package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxCurrentModelCatalogRows       = 10_000
	currentModelCatalogQueryRows     = maxCurrentModelCatalogRows + 1
	maxCurrentModelCatalogBytes      = 8 * 1024 * 1024
	currentLLMDefaultMaxOutputTokens = 16_000
)

var (
	// errInvalidCurrentModelConfiguration reports a row whose `data` document
	// has the wrong SHAPE: a value that is not an object, a missing name, or a
	// field with the wrong JSON type. The row is bad; the query is not. List
	// skips such a row, because one bad row must not remove the whole
	// catalogue.
	errInvalidCurrentModelConfiguration = errors.New("current model configuration is invalid")
	// errCurrentModelRowNotOwned reports a row that breaks a tenant invariant:
	// a foreign project, a foreign section, or a private row in a shared-only
	// read. The row content is not the problem. The query or the transaction
	// returned a row it must never return, so List fails closed and returns
	// nothing.
	errCurrentModelRowNotOwned     = errors.New("current model row does not belong to the requested scope")
	errCurrentModelCatalogTooLarge = errors.New("current model catalog exceeds the safe row limit")
)

type currentModelQueries interface {
	GetCurrentModelCatalogBounds(context.Context, sqlcgen.GetCurrentModelCatalogBoundsParams) (sqlcgen.GetCurrentModelCatalogBoundsRow, error)
	ListCurrentModelConfigurations(context.Context, sqlcgen.ListCurrentModelConfigurationsParams) ([]sqlcgen.ListCurrentModelConfigurationsRow, error)
}

type currentModelQueryFactory func(sqlExecutor) (currentModelQueries, error)

// CurrentModelsRepository reads model candidates from one authorized tenant
// schema. Provider-specific validation and credential expansion remain outside
// this adapter.
type CurrentModelsRepository struct {
	projects projectStore
	queries  currentModelQueryFactory
}

func NewCurrentModelsRepository(pool *pgxpool.Pool) (*CurrentModelsRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return newCurrentModelsRepository(projects, newCurrentModelQueries)
}

func newCurrentModelsRepository(projects projectStore, queries currentModelQueryFactory) (*CurrentModelsRepository, error) {
	if projects == nil || queries == nil {
		return nil, errors.New("current model database is required")
	}
	return &CurrentModelsRepository{projects: projects, queries: queries}, nil
}

func newCurrentModelQueries(tx sqlExecutor) (currentModelQueries, error) {
	executor, ok := tx.(pgxExecutor)
	if !ok || executor.queryer == nil {
		return nil, errors.New("current model transaction does not support generated queries")
	}
	return sqlcgen.New(executor.queryer), nil
}

// List returns the ordered candidates consumed by BuildCurrentModelCatalog.
// sharedOnly is used for the public-project query and is enforced in SQL.
func (r *CurrentModelsRepository) List(
	ctx context.Context,
	projectID int32,
	section configurationapp.CurrentModelSection,
	sharedOnly bool,
) ([]configurationapp.CurrentModelCatalogItem, error) {
	if err := validateCurrentModelRepositoryRequest(ctx, projectID, section); err != nil {
		return nil, err
	}

	rows := []sqlcgen.ListCurrentModelConfigurationsRow{}
	err := r.projects.WithinProjectTx(ctx, int64(projectID), pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	}, func(tx sqlExecutor) error {
		queries, err := r.queries(tx)
		if err != nil {
			return err
		}
		bounds, err := queries.GetCurrentModelCatalogBounds(ctx, sqlcgen.GetCurrentModelCatalogBoundsParams{
			ProjectID:  projectID,
			Section:    string(section),
			SharedOnly: sharedOnly,
			LimitRows:  currentModelCatalogQueryRows,
		})
		if err != nil {
			return fmt.Errorf("bound current model configurations: %w", err)
		}
		if bounds.RowCount > maxCurrentModelCatalogRows ||
			bounds.ProjectedBytes > maxCurrentModelCatalogBytes ||
			bounds.RowCount < 0 ||
			bounds.ProjectedBytes < 0 {
			return errCurrentModelCatalogTooLarge
		}
		rows, err = queries.ListCurrentModelConfigurations(ctx, sqlcgen.ListCurrentModelConfigurationsParams{
			ProjectID:  projectID,
			Section:    string(section),
			SharedOnly: sharedOnly,
			LimitRows:  currentModelCatalogQueryRows,
		})
		if err != nil {
			return fmt.Errorf("list current model configurations: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(rows) > maxCurrentModelCatalogRows {
		return nil, errCurrentModelCatalogTooLarge
	}
	// This is a defensive invariant check, not the memory bound. The generated
	// aggregate query above rejects an oversized snapshot before any JSONB row
	// is transferred into this process, and REPEATABLE READ keeps the checked
	// and projected snapshots identical.
	totalBytes := 0
	for _, row := range rows {
		rowBytes := len(row.Data) + len(row.EliteaTitle) + len(row.Section)
		if row.Label != nil {
			rowBytes += len(*row.Label)
		}
		if rowBytes > maxCurrentModelCatalogBytes-totalBytes {
			return nil, errCurrentModelCatalogTooLarge
		}
		totalBytes += rowBytes
	}

	candidates := make([]currentModelCandidate, 0, len(rows))
	for _, row := range rows {
		candidate, err := mapCurrentModelCandidate(row, projectID, section, sharedOnly)
		if errors.Is(err, errInvalidCurrentModelConfiguration) {
			// Skip the row. Do not fail the read.
			//
			// The write path stores `data` without a registry-schema check.
			// One editor can therefore store a wrongly typed field through the
			// public API. `"context_window":"128000"` is the plain case.
			// An abort here turned that single row into a 500 for GET
			// /api/v2/configurations/models/{projectID}. The model picker was
			// then empty for every member of the project, until someone
			// repaired the row.
			//
			// Log the identifiers only. The value is a stored configuration and
			// may hold a credential reference, so it never reaches a log line
			// or an error message.
			slog.WarnContext(ctx, "skipping a malformed model configuration",
				"project_id", projectID,
				"configuration_id", row.ID,
				"section", string(section))
			continue
		}
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	if section == configurationapp.CurrentModelSectionLLM {
		orderCurrentLLMDuplicates(candidates)
	}

	items := make([]configurationapp.CurrentModelCatalogItem, len(candidates))
	for index := range candidates {
		items[index] = candidates[index].item
	}
	return items, nil
}

type currentModelCandidate struct {
	id   int32
	item configurationapp.CurrentModelCatalogItem
}

func mapCurrentModelCandidate(
	row sqlcgen.ListCurrentModelConfigurationsRow,
	projectID int32,
	section configurationapp.CurrentModelSection,
	sharedOnly bool,
) (currentModelCandidate, error) {
	// A tenant-invariant break is NOT a skippable row. It means the query or
	// the transaction handed back a row from another scope, so the whole read
	// is untrustworthy.
	if row.ID <= 0 || row.ProjectID != projectID || row.Section != string(section) || (sharedOnly && !row.Shared) {
		return currentModelCandidate{}, errCurrentModelRowNotOwned
	}

	item := configurationapp.CurrentModelCatalogItem{
		ProjectID: row.ProjectID,
		Shared:    row.Shared,
	}
	// The GRANT is read for the cross-project read alone.
	//
	// It decides whether a CATALOGUE row is offered to the caller, and
	// sharedOnly is true for exactly that read. A project's own rows are its
	// own whatever their `data` says, so reading the field there would cost one
	// more decode per row of the hot path to answer a question nobody asks. The
	// zero value is "every project", which is what an own row is.
	//
	// The decode is deliberately its own, tolerant pass rather than a field on
	// the strict document below: a row whose `share_scope` is malformed keeps
	// behaving as it did before the field existed (model_grant.go), where a
	// strict decode would drop the whole row out of the catalogue instead.
	if sharedOnly {
		var grantData map[string]any
		_ = json.Unmarshal(row.Data, &grantData)
		item.Grant = configurationapp.ReadModelGrant(grantData)
	}
	if section == configurationapp.CurrentModelSectionVectorStorage {
		item.Name = row.EliteaTitle
		return currentModelCandidate{id: row.ID, item: item}, nil
	}
	if row.Label == nil {
		return currentModelCandidate{}, errInvalidCurrentModelConfiguration
	}

	data, err := decodeCurrentModelData(row.Data)
	if err != nil {
		return currentModelCandidate{}, err
	}
	name, err := requiredCurrentModelString(data, "name")
	if err != nil {
		return currentModelCandidate{}, err
	}
	displayName := *row.Label
	item.Name = name
	item.DisplayName = &displayName
	if section != configurationapp.CurrentModelSectionLLM {
		return currentModelCandidate{id: row.ID, item: item}, nil
	}

	item.ContextWindow, err = optionalCurrentModelInt(data, "context_window")
	if err != nil {
		return currentModelCandidate{}, err
	}
	item.MaxOutputTokens, err = optionalCurrentModelInt(data, "max_output_tokens")
	if err != nil {
		return currentModelCandidate{}, err
	}
	item.SupportsReasoning, err = optionalCurrentModelBool(data, "supports_reasoning")
	if err != nil {
		return currentModelCandidate{}, err
	}
	item.SupportsVision, err = optionalCurrentModelBool(data, "supports_vision")
	if err != nil {
		return currentModelCandidate{}, err
	}
	item.LowTier, err = optionalCurrentModelBool(data, "low_tier")
	if err != nil {
		return currentModelCandidate{}, err
	}
	item.HighTier, err = optionalCurrentModelBoolWithFallback(data, "high_tier", "mid_tier")
	if err != nil {
		return currentModelCandidate{}, err
	}
	item.OpenAICompatible, err = optionalCurrentModelBool(data, "openai_compatible")
	if err != nil {
		return currentModelCandidate{}, err
	}
	return currentModelCandidate{id: row.ID, item: item}, nil
}

func decodeCurrentModelData(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var data map[string]json.RawMessage
	if err := decoder.Decode(&data); err != nil || data == nil {
		return nil, errInvalidCurrentModelConfiguration
	}
	if err := requireCurrentModelJSONEOF(decoder); err != nil {
		return nil, err
	}
	return data, nil
}

func requireCurrentModelJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errInvalidCurrentModelConfiguration
	}
	return nil
}

func requiredCurrentModelString(data map[string]json.RawMessage, key string) (string, error) {
	raw, ok := data[key]
	if !ok || currentModelJSONNull(raw) {
		return "", errInvalidCurrentModelConfiguration
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errInvalidCurrentModelConfiguration
	}
	return value, nil
}

// optionalCurrentModelInt reads one integer field from a stored `data`
// document. It accepts a JSON number AND a JSON string that holds a decimal
// integer.
//
// The string form is not a theoretical case. The AI-Configuration form
// classified `max_output_tokens` as a secret, because the key contains the
// substring `token`, and a masked text input serialises its value as a string:
// every model saved through that form stored `"max_output_tokens": "16000"`.
// This reader refused the value, mapCurrentModelCandidate skipped the whole
// row, and the model never appeared in any picker — one wrongly typed optional
// field removed a model that was otherwise complete and correct.
//
// The form is fixed (apps/elitea-web features/credentials/lib/schemaField.ts),
// but the rows it already wrote are still in the database, and no other
// component rewrites them. Coercing here recovers them without a migration.
//
// A value that is not an integer at all — "", "abc", "1.5", true, an object —
// is still refused, so the row is still skipped and still warned about. This
// widens what counts as well-formed; it does not stop reporting malformed.
func optionalCurrentModelInt(data map[string]json.RawMessage, key string) (*int, error) {
	raw, ok := data[key]
	if !ok || currentModelJSONNull(raw) {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, errInvalidCurrentModelConfiguration
	}
	literal, err := currentModelIntegerLiteral(value)
	if err != nil {
		return nil, err
	}
	parsed, err := strconv.ParseInt(literal, 10, strconv.IntSize)
	if err != nil {
		return nil, errInvalidCurrentModelConfiguration
	}
	result := int(parsed)
	return &result, nil
}

// currentModelIntegerLiteral returns the decimal text a decoded JSON value
// carries, for the two shapes an integer field is stored in: a number, and a
// quoted number. Surrounding whitespace inside the string is trimmed; nothing
// else about the text is rewritten, so ParseInt stays the single authority on
// what an integer is.
func currentModelIntegerLiteral(value any) (string, error) {
	switch typed := value.(type) {
	case json.Number:
		return typed.String(), nil
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return "", errInvalidCurrentModelConfiguration
		}
		return trimmed, nil
	default:
		return "", errInvalidCurrentModelConfiguration
	}
}

func optionalCurrentModelBool(data map[string]json.RawMessage, key string) (*bool, error) {
	raw, ok := data[key]
	if !ok || currentModelJSONNull(raw) {
		return nil, nil
	}
	return decodeCurrentModelBool(raw)
}

func decodeCurrentModelBool(raw json.RawMessage) (*bool, error) {
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errInvalidCurrentModelConfiguration
	}
	return &value, nil
}

func optionalCurrentModelBoolWithFallback(data map[string]json.RawMessage, key, fallbackKey string) (*bool, error) {
	if raw, ok := data[key]; ok && !currentModelJSONNull(raw) {
		return decodeCurrentModelBool(raw)
	}
	return optionalCurrentModelBool(data, fallbackKey)
}

func currentModelJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// The current SQLAlchemy query selects the greatest max_output_tokens for a
// repeated LLM name. We keep SQL provider-neutral and reproduce that ordering
// after strict JSON decoding, so malformed values never reach a PostgreSQL cast
// error that could echo their contents. The pure application seam then keeps
// the last candidate for each project/name key.
func orderCurrentLLMDuplicates(candidates []currentModelCandidate) {
	positionsByName := make(map[string][]int, len(candidates))
	for index := range candidates {
		positionsByName[candidates[index].item.Name] = append(positionsByName[candidates[index].item.Name], index)
	}
	for _, positions := range positionsByName {
		if len(positions) < 2 {
			continue
		}
		group := make([]currentModelCandidate, len(positions))
		for index, position := range positions {
			group[index] = candidates[position]
		}
		sort.SliceStable(group, func(left, right int) bool {
			leftMax := currentModelEffectiveMaxOutput(group[left].item)
			rightMax := currentModelEffectiveMaxOutput(group[right].item)
			if leftMax != rightMax {
				return leftMax < rightMax
			}
			return group[left].id < group[right].id
		})
		for index, position := range positions {
			candidates[position] = group[index]
		}
	}
}

func currentModelEffectiveMaxOutput(item configurationapp.CurrentModelCatalogItem) int {
	if item.MaxOutputTokens == nil {
		return currentLLMDefaultMaxOutputTokens
	}
	return *item.MaxOutputTokens
}

func validateCurrentModelRepositoryRequest(ctx context.Context, projectID int32, section configurationapp.CurrentModelSection) error {
	if ctx == nil || projectID <= 0 || !configurationapp.IsSupportedCurrentModelSection(section) {
		return configurationapp.ErrInvalidCurrentConfigurationRequest
	}
	return ctx.Err()
}
