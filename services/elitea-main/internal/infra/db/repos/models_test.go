package repos

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

func TestCurrentModelsRepositoryRoutesAuthorizedTenantQuery(t *testing.T) {
	label := "Embedding One"
	queries := &currentModelQueriesStub{rows: []sqlcgen.ListCurrentModelConfigurationsRow{
		currentModelRow(11, 7, configurationapp.CurrentModelSectionEmbedding, label, false, `{"name":"embedding-one"}`),
	}}
	projects := &currentModelProjectStore{}
	repository := newCurrentModelsRepositoryForTest(t, projects, queries)

	items, err := repository.List(context.Background(), 7, configurationapp.CurrentModelSectionEmbedding, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "embedding-one" || items[0].DisplayName == nil ||
		*items[0].DisplayName != label || items[0].ProjectID != 7 || items[0].Shared {
		t.Fatalf("mapped items=%#v", items)
	}
	if !reflect.DeepEqual(projects.projectIDs, []int64{7}) || len(projects.options) != 1 ||
		projects.options[0].IsoLevel != pgx.RepeatableRead || projects.options[0].AccessMode != pgx.ReadOnly {
		t.Fatalf("project transaction ids=%v options=%#v", projects.projectIDs, projects.options)
	}
	wantBounds := sqlcgen.GetCurrentModelCatalogBoundsParams{
		ProjectID: 7, Section: "embedding", SharedOnly: false, LimitRows: currentModelCatalogQueryRows,
	}
	wantParams := sqlcgen.ListCurrentModelConfigurationsParams{
		ProjectID: 7, Section: "embedding", SharedOnly: false, LimitRows: currentModelCatalogQueryRows,
	}
	if queries.boundCalls != 1 || queries.listCalls != 1 ||
		!reflect.DeepEqual(queries.boundParams, wantBounds) ||
		!reflect.DeepEqual(queries.params, wantParams) {
		t.Fatalf(
			"bound calls=%d params=%#v list calls=%d params=%#v",
			queries.boundCalls,
			queries.boundParams,
			queries.listCalls,
			queries.params,
		)
	}
}

func TestCurrentModelsRepositoryPreservesSectionShapesAndPublicSharedFilter(t *testing.T) {
	tests := []struct {
		name       string
		section    configurationapp.CurrentModelSection
		row        sqlcgen.ListCurrentModelConfigurationsRow
		sharedOnly bool
		assert     func(*testing.T, configurationapp.CurrentModelCatalogItem)
	}{
		{
			name:    "vector storage uses title without parsing unrelated data",
			section: configurationapp.CurrentModelSectionVectorStorage,
			row:     currentModelRow(1, 7, configurationapp.CurrentModelSectionVectorStorage, "", false, `[]`),
			assert: func(t *testing.T, item configurationapp.CurrentModelCatalogItem) {
				t.Helper()
				if item.Name != "title-1" || item.DisplayName != nil {
					t.Fatalf("vector storage item=%#v", item)
				}
			},
		},
		{
			name:       "public shared model remains marked shared",
			section:    configurationapp.CurrentModelSectionTTS,
			row:        currentModelRow(2, 1, configurationapp.CurrentModelSectionTTS, "Voice", true, `{"name":"voice-one"}`),
			sharedOnly: true,
			assert: func(t *testing.T, item configurationapp.CurrentModelCatalogItem) {
				t.Helper()
				if item.Name != "voice-one" || item.DisplayName == nil || *item.DisplayName != "Voice" || !item.Shared {
					t.Fatalf("shared TTS item=%#v", item)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			queries := &currentModelQueriesStub{rows: []sqlcgen.ListCurrentModelConfigurationsRow{test.row}}
			repository := newCurrentModelsRepositoryForTest(t, &currentModelProjectStore{}, queries)
			items, err := repository.List(context.Background(), test.row.ProjectID, test.section, test.sharedOnly)
			if err != nil || len(items) != 1 {
				t.Fatalf("items=%#v err=%v", items, err)
			}
			test.assert(t, items[0])
			if queries.params.SharedOnly != test.sharedOnly {
				t.Fatalf("shared-only query=%#v", queries.params)
			}
		})
	}
}

func TestCurrentModelsRepositoryOrdersLLMDuplicatesForCurrentMaxTokenSelection(t *testing.T) {
	queries := &currentModelQueriesStub{rows: []sqlcgen.ListCurrentModelConfigurationsRow{
		currentModelRow(1, 7, configurationapp.CurrentModelSectionLLM, "Alpha Max", false,
			`{"name":"alpha","context_window":200000,"max_output_tokens":32000,"supports_reasoning":true,"supports_vision":false,"low_tier":true,"mid_tier":true,"openai_compatible":true}`),
		currentModelRow(2, 7, configurationapp.CurrentModelSectionLLM, "Beta", false,
			`{"name":"beta","max_output_tokens":8000}`),
		currentModelRow(3, 7, configurationapp.CurrentModelSectionLLM, "Alpha Newer But Smaller", false,
			`{"name":"alpha","max_output_tokens":16000,"high_tier":false,"mid_tier":true}`),
	}}
	repository := newCurrentModelsRepositoryForTest(t, &currentModelProjectStore{}, queries)

	items, err := repository.List(context.Background(), 7, configurationapp.CurrentModelSectionLLM, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].Name != "alpha" || items[0].MaxOutputTokens == nil || *items[0].MaxOutputTokens != 16000 ||
		items[1].Name != "beta" || items[2].Name != "alpha" || items[2].MaxOutputTokens == nil || *items[2].MaxOutputTokens != 32000 {
		t.Fatalf("ordered LLM candidates=%#v", items)
	}
	if items[0].HighTier == nil || *items[0].HighTier {
		t.Fatalf("explicit high-tier=false did not override mid-tier fallback: %#v", items[0])
	}
	if items[2].HighTier == nil || !*items[2].HighTier || items[2].SupportsReasoning == nil || !*items[2].SupportsReasoning ||
		items[2].SupportsVision == nil || *items[2].SupportsVision || items[2].LowTier == nil || !*items[2].LowTier ||
		items[2].OpenAICompatible == nil || !*items[2].OpenAICompatible || items[2].ContextWindow == nil || *items[2].ContextWindow != 200000 {
		t.Fatalf("LLM fields=%#v", items[2])
	}

	response := configurationapp.BuildCurrentModelCatalog(configurationapp.CurrentModelCatalogRequest{
		Section:      configurationapp.CurrentModelSectionLLM,
		ProjectID:    7,
		ProjectItems: items,
	})
	if response.Total != 2 || response.DefaultModelName == nil || *response.DefaultModelName != "alpha" {
		t.Fatalf("catalog response=%#v", response)
	}
	var alpha configurationapp.CurrentModelCatalogItem
	for _, item := range response.Items {
		if item.Name == "alpha" {
			alpha = item
		}
	}
	if alpha.MaxOutputTokens == nil || *alpha.MaxOutputTokens != 32000 || alpha.DisplayName == nil || *alpha.DisplayName != "Alpha Max" {
		t.Fatalf("deduplicated max-token LLM=%#v", alpha)
	}
}

func TestCurrentModelsRepositoryUsesLLMDefaultMaxForDuplicateOrdering(t *testing.T) {
	queries := &currentModelQueriesStub{rows: []sqlcgen.ListCurrentModelConfigurationsRow{
		currentModelRow(1, 7, configurationapp.CurrentModelSectionLLM, "Explicit Small", false,
			`{"name":"alpha","max_output_tokens":8000}`),
		currentModelRow(2, 7, configurationapp.CurrentModelSectionLLM, "Implicit Default", false,
			`{"name":"alpha"}`),
	}}
	repository := newCurrentModelsRepositoryForTest(t, &currentModelProjectStore{}, queries)
	items, err := repository.List(context.Background(), 7, configurationapp.CurrentModelSectionLLM, false)
	if err != nil || len(items) != 2 {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	if items[1].MaxOutputTokens != nil || items[1].DisplayName == nil || *items[1].DisplayName != "Implicit Default" {
		t.Fatalf("implicit current default did not win=%#v", items)
	}
}

// A row that breaks a tenant invariant fails the whole read.
//
// These rows say the query or the transaction returned something from another
// scope. The read is then untrustworthy, so it must return nothing rather than
// a smaller answer.
func TestCurrentModelsRepositoryRejectsRowsFromAnotherScope(t *testing.T) {
	valid := currentModelRow(1, 7, configurationapp.CurrentModelSectionLLM, "Model", true,
		`{"name":"model","context_window":128000,"max_output_tokens":16000}`)
	tests := []struct {
		name       string
		row        sqlcgen.ListCurrentModelConfigurationsRow
		sharedOnly bool
	}{
		{name: "wrong project", row: func() sqlcgen.ListCurrentModelConfigurationsRow { row := valid; row.ProjectID = 8; return row }()},
		{name: "wrong section", row: func() sqlcgen.ListCurrentModelConfigurationsRow { row := valid; row.Section = "embedding"; return row }()},
		{name: "public row not shared", sharedOnly: true, row: func() sqlcgen.ListCurrentModelConfigurationsRow { row := valid; row.Shared = false; return row }()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			queries := &currentModelQueriesStub{rows: []sqlcgen.ListCurrentModelConfigurationsRow{test.row}}
			repository := newCurrentModelsRepositoryForTest(t, &currentModelProjectStore{}, queries)
			_, err := repository.List(context.Background(), 7, configurationapp.CurrentModelSectionLLM, test.sharedOnly)
			if !errors.Is(err, errCurrentModelRowNotOwned) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

// THE DEFECT. One row with a wrongly typed `data` field removed the WHOLE model
// catalogue of the project.
//
// List mapped each row and returned on the first error.
// optionalCurrentModelInt/Bool reject a field whose JSON type is wrong. An
// example is `"context_window":"128000"` as a string. The compatibility write path
// (internal/api/v2/configurations/handler.go) stores `data` with no
// registry-schema check, so any project editor can store such a row through the
// public API. Every later GET /api/v2/configurations/models/{projectID} then
// answered 500 for every member of that project, and the chat model picker was
// empty until someone repaired the row.
//
// A malformed row must be skipped instead. The good rows still reach the
// caller.
func TestCurrentModelsRepositorySkipsMalformedRowsAndKeepsTheGoodOnes(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "missing display name", data: `{"name":"model"}`},
		{name: "data is not object", data: `[]`},
		{name: "name has wrong type", data: `{"name":7}`},
		// A quoted DECIMAL INTEGER is no longer malformed — see
		// TestCurrentModelsRepositoryReadsIntegerFieldsStoredAsStrings. These
		// four are still malformed, because none of them is an integer at all.
		{name: "non-numeric string", data: `{"name":"model","max_output_tokens":"private-secret"}`},
		{name: "empty string", data: `{"name":"model","context_window":""}`},
		{name: "fractional string", data: `{"name":"model","context_window":"128000.5"}`},
		{name: "integer field sent as a bool", data: `{"name":"model","context_window":true}`},
		{name: "boolean field sent as a string", data: `{"name":"model","supports_vision":"private-secret"}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bad := currentModelRow(2, 7, configurationapp.CurrentModelSectionLLM, "Broken", false, test.data)
			if test.name == "missing display name" {
				bad.Label = nil
			}
			queries := &currentModelQueriesStub{rows: []sqlcgen.ListCurrentModelConfigurationsRow{
				currentModelRow(1, 7, configurationapp.CurrentModelSectionLLM, "Good One", false,
					`{"name":"good-one","context_window":128000}`),
				bad,
				currentModelRow(3, 7, configurationapp.CurrentModelSectionLLM, "Good Two", false,
					`{"name":"good-two","context_window":64000}`),
			}}
			repository := newCurrentModelsRepositoryForTest(t, &currentModelProjectStore{}, queries)
			items, err := repository.List(context.Background(), 7, configurationapp.CurrentModelSectionLLM, false)
			if err != nil {
				t.Fatalf("one malformed row removed the whole catalogue: %v", err)
			}
			if len(items) != 2 || items[0].Name != "good-one" || items[1].Name != "good-two" {
				t.Fatalf("items=%#v", items)
			}
		})
	}
}

// The AI-Configuration form classified `max_output_tokens` as a SECRET,
// because the property key contains the substring `token`
// (apps/elitea-web src/features/credentials/lib/schemaField.ts). A masked text
// input serialises a string, so every model saved through that form stored
// `"max_output_tokens": "16000"`.
//
// optionalCurrentModelInt refused the quoted number, mapCurrentModelCandidate
// skipped the row, and the model was absent from every model picker while the
// row itself was complete and correct. One optional field with the wrong JSON
// type removed the whole model.
//
// The form is fixed. The rows it already wrote are not, and nothing rewrites
// them, so the reader coerces a quoted decimal integer instead of dropping the
// row. The value must be the SAME as the unquoted form: a coercion that
// produced a different number would be worse than the refusal.
func TestCurrentModelsRepositoryReadsIntegerFieldsStoredAsStrings(t *testing.T) {
	queries := &currentModelQueriesStub{rows: []sqlcgen.ListCurrentModelConfigurationsRow{
		currentModelRow(1, 7, configurationapp.CurrentModelSectionLLM, "Quoted", false,
			`{"name":"quoted","context_window":"128000","max_output_tokens":" 16000 "}`),
		currentModelRow(2, 7, configurationapp.CurrentModelSectionLLM, "Unquoted", false,
			`{"name":"unquoted","context_window":128000,"max_output_tokens":16000}`),
	}}
	repository := newCurrentModelsRepositoryForTest(t, &currentModelProjectStore{}, queries)

	items, err := repository.List(context.Background(), 7, configurationapp.CurrentModelSectionLLM, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("a row whose integer field is a quoted number was skipped: items=%#v", items)
	}
	for _, item := range items {
		if item.ContextWindow == nil || *item.ContextWindow != 128000 {
			t.Fatalf("%s context_window=%v, want 128000", item.Name, item.ContextWindow)
		}
		if item.MaxOutputTokens == nil || *item.MaxOutputTokens != 16000 {
			t.Fatalf("%s max_output_tokens=%v, want 16000", item.Name, item.MaxOutputTokens)
		}
	}
}

// A negative and a plus-signed literal are integers in both shapes. This pins
// that the coercion adds no rewriting of its own: strconv.ParseInt stays the
// single authority on what an integer is, quoted or not.
func TestOptionalCurrentModelIntAcceptsTheSameLiteralsQuotedAndUnquoted(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
		ok   bool
	}{
		{name: "number", raw: `16000`, want: 16000, ok: true},
		{name: "quoted number", raw: `"16000"`, want: 16000, ok: true},
		{name: "quoted number with spaces", raw: `"  16000  "`, want: 16000, ok: true},
		{name: "quoted negative", raw: `"-1"`, want: -1, ok: true},
		{name: "quoted plus sign", raw: `"+7"`, want: 7, ok: true},
		{name: "quoted empty", raw: `""`},
		{name: "quoted words", raw: `"sixteen thousand"`},
		{name: "quoted fraction", raw: `"1.5"`},
		{name: "unquoted fraction", raw: `1.5`},
		{name: "bool", raw: `true`},
		{name: "object", raw: `{"value":1}`},
		{name: "array", raw: `[1]`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := map[string]json.RawMessage{"max_output_tokens": json.RawMessage(test.raw)}
			got, err := optionalCurrentModelInt(data, "max_output_tokens")
			if !test.ok {
				if !errors.Is(err, errInvalidCurrentModelConfiguration) {
					t.Fatalf("value=%v err=%v, want the malformed-configuration error", got, err)
				}
				return
			}
			if err != nil || got == nil || *got != test.want {
				t.Fatalf("value=%v err=%v, want %d", got, err, test.want)
			}
		})
	}

	// Absent and JSON null both stay "no value", not an error: the field is
	// optional and always was.
	for _, data := range []map[string]json.RawMessage{
		{},
		{"max_output_tokens": json.RawMessage(`null`)},
	} {
		got, err := optionalCurrentModelInt(data, "max_output_tokens")
		if err != nil || got != nil {
			t.Fatalf("absent/null value=%v err=%v, want nil/nil", got, err)
		}
	}
}

func TestCurrentModelsRepositoryValidatesRequestAndBoundsCatalog(t *testing.T) {
	queries := &currentModelQueriesStub{}
	projects := &currentModelProjectStore{}
	repository := newCurrentModelsRepositoryForTest(t, projects, queries)

	requests := []struct {
		ctx       context.Context
		projectID int32
		section   configurationapp.CurrentModelSection
	}{
		{ctx: nil, projectID: 7, section: configurationapp.CurrentModelSectionLLM},
		{ctx: context.Background(), projectID: 0, section: configurationapp.CurrentModelSectionLLM},
		{ctx: context.Background(), projectID: 7, section: configurationapp.CurrentModelSection("unknown")},
	}
	for _, request := range requests {
		_, err := repository.List(request.ctx, request.projectID, request.section, false)
		if !errors.Is(err, configurationapp.ErrInvalidCurrentConfigurationRequest) {
			t.Fatalf("invalid request error=%v", err)
		}
	}
	if queries.boundCalls != 0 || queries.listCalls != 0 || len(projects.projectIDs) != 0 {
		t.Fatalf(
			"invalid request reached database: bound=%d list=%d projects=%v",
			queries.boundCalls,
			queries.listCalls,
			projects.projectIDs,
		)
	}

	label := "Model"
	queries.rows = make([]sqlcgen.ListCurrentModelConfigurationsRow, currentModelCatalogQueryRows)
	for index := range queries.rows {
		queries.rows[index] = currentModelRow(int32(index+1), 7, configurationapp.CurrentModelSectionEmbedding, label, false, `{"name":"model"}`)
	}
	_, err := repository.List(context.Background(), 7, configurationapp.CurrentModelSectionEmbedding, false)
	if !errors.Is(err, errCurrentModelCatalogTooLarge) {
		t.Fatalf("catalog limit error=%v", err)
	}
	if queries.listCalls != 0 {
		t.Fatalf("oversized row catalog was materialized: list calls=%d", queries.listCalls)
	}

	oversizedData := `{"name":"model","padding":"` +
		strings.Repeat("x", maxCurrentModelCatalogBytes) + `"}`
	queries.rows = []sqlcgen.ListCurrentModelConfigurationsRow{
		currentModelRow(1, 7, configurationapp.CurrentModelSectionEmbedding, label, false, oversizedData),
	}
	_, err = repository.List(context.Background(), 7, configurationapp.CurrentModelSectionEmbedding, false)
	if !errors.Is(err, errCurrentModelCatalogTooLarge) {
		t.Fatalf("catalog byte limit error=%v", err)
	}
	if queries.listCalls != 0 {
		t.Fatalf("oversized JSONB row was materialized: list calls=%d", queries.listCalls)
	}
}

func TestNewCurrentModelsRepositoryRejectsMissingDependencies(t *testing.T) {
	if _, err := NewCurrentModelsRepository(nil); err == nil {
		t.Fatal("nil database pool was accepted")
	}
	if _, err := newCurrentModelsRepository(nil, func(sqlExecutor) (currentModelQueries, error) { return &currentModelQueriesStub{}, nil }); err == nil {
		t.Fatal("nil project store was accepted")
	}
	if _, err := newCurrentModelsRepository(&currentModelProjectStore{}, nil); err == nil {
		t.Fatal("nil query factory was accepted")
	}
}

func newCurrentModelsRepositoryForTest(t *testing.T, projects projectStore, queries currentModelQueries) *CurrentModelsRepository {
	t.Helper()
	repository, err := newCurrentModelsRepository(projects, func(sqlExecutor) (currentModelQueries, error) {
		return queries, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func currentModelRow(
	id, projectID int32,
	section configurationapp.CurrentModelSection,
	label string,
	shared bool,
	data string,
) sqlcgen.ListCurrentModelConfigurationsRow {
	var displayName *string
	if label != "" {
		value := label
		displayName = &value
	}
	return sqlcgen.ListCurrentModelConfigurationsRow{
		ID: id, ProjectID: projectID, Label: displayName, EliteaTitle: "title-" + strconv.FormatInt(int64(id), 10),
		Section: string(section), Data: []byte(data), Shared: shared,
	}
}

type currentModelProjectStore struct {
	projectIDs []int64
	options    []pgx.TxOptions
}

func (s *currentModelProjectStore) WithinProjectTx(_ context.Context, projectID int64, options pgx.TxOptions, fn func(sqlExecutor) error) error {
	s.projectIDs = append(s.projectIDs, projectID)
	s.options = append(s.options, options)
	return fn(nil)
}

type currentModelQueriesStub struct {
	rows        []sqlcgen.ListCurrentModelConfigurationsRow
	err         error
	boundParams sqlcgen.GetCurrentModelCatalogBoundsParams
	params      sqlcgen.ListCurrentModelConfigurationsParams
	boundCalls  int
	listCalls   int
}

func (s *currentModelQueriesStub) GetCurrentModelCatalogBounds(
	_ context.Context,
	params sqlcgen.GetCurrentModelCatalogBoundsParams,
) (sqlcgen.GetCurrentModelCatalogBoundsRow, error) {
	s.boundCalls++
	s.boundParams = params
	if s.err != nil {
		return sqlcgen.GetCurrentModelCatalogBoundsRow{}, s.err
	}
	rows := s.rows
	if len(rows) > int(params.LimitRows) {
		rows = rows[:params.LimitRows]
	}
	var bounds sqlcgen.GetCurrentModelCatalogBoundsRow
	bounds.RowCount = int64(len(rows))
	for _, row := range rows {
		projected := int64(len(row.Data) + len(row.EliteaTitle) + len(row.Section))
		if row.Label != nil {
			projected += int64(len(*row.Label))
		}
		bounds.ProjectedBytes += projected
	}
	return bounds, nil
}

func (s *currentModelQueriesStub) ListCurrentModelConfigurations(
	_ context.Context,
	params sqlcgen.ListCurrentModelConfigurationsParams,
) ([]sqlcgen.ListCurrentModelConfigurationsRow, error) {
	s.listCalls++
	s.params = params
	return s.rows, s.err
}
