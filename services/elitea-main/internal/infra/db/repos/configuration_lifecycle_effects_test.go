package repos

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

const currentConfigurationLifecycleEffectsVersion = "0123456789abcdef0123456789abcdef"

func TestCurrentConfigurationLifecycleEffectsStatusUsesExactTenantFence(t *testing.T) {
	queries := &currentConfigurationLifecycleEffectsProjectQueriesStub{statusRows: 1}
	projects := &currentConfigurationLifecycleEffectsProjectStore{}
	repository := mustCurrentConfigurationLifecycleEffectsRepository(t, projects, queries, &currentConfigurationLifecycleEffectsSharedQueriesStub{})
	target := configurationapp.CurrentConfigurationLifecycleStatusTarget{
		ProjectID:         7,
		ConfigurationID:   41,
		ConfigurationUUID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		StatusOK:          true,
	}

	updated, err := repository.SetCurrentConfigurationLifecycleStatus(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !updated || queries.statusCalls != 1 {
		t.Fatalf("updated=%t calls=%d", updated, queries.statusCalls)
	}
	if queries.statusParams != (sqlcgen.SetCurrentConfigurationLifecycleStatusParams{
		StatusOk: true, ProjectID: 7, ConfigurationID: 41,
		ConfigurationUuid: target.ConfigurationUUID,
	}) {
		t.Fatalf("params=%+v", queries.statusParams)
	}
	assertCurrentConfigurationLifecycleProjectTx(t, projects, 7, pgx.ReadWrite)

	queries.statusRows = 0
	updated, err = repository.SetCurrentConfigurationLifecycleStatus(context.Background(), target)
	if err != nil || updated {
		t.Fatalf("stale update=%t error=%v", updated, err)
	}
	queries.statusRows = 2
	if _, err := repository.SetCurrentConfigurationLifecycleStatus(context.Background(), target); !errors.Is(err, configurationapp.ErrCurrentConfigurationLifecycleInternalUnavailable) {
		t.Fatalf("multi-row error=%v", err)
	}
}

func TestCurrentConfigurationLifecycleEffectsRenameScanIsBoundedAndDetached(t *testing.T) {
	first := []byte(`{"nested":{"elitea_title":"before","private":true}}`)
	second := []byte(`{"unchanged":true}`)
	queries := &currentConfigurationLifecycleEffectsProjectQueriesStub{
		listRows: []sqlcgen.ListCurrentConfigurationRenameToolkitsRow{
			{ID: 2, SettingsVersion: currentConfigurationLifecycleEffectsVersion, Settings: first, SettingsBytes: int64(len(first)), TotalBytes: int64(len(first))},
			{ID: 5, SettingsVersion: "abcdefabcdefabcdefabcdefabcdefab", Settings: second, SettingsBytes: int64(len(second)), TotalBytes: int64(len(first) + len(second))},
		},
	}
	projects := &currentConfigurationLifecycleEffectsProjectStore{}
	repository := mustCurrentConfigurationLifecycleEffectsRepository(t, projects, queries, &currentConfigurationLifecycleEffectsSharedQueriesStub{})
	limits := configurationapp.CurrentConfigurationRenameScanLimits{MaxRows: 3, MaxSettingsBytes: 1024, MaxTotalBytes: 2048}

	toolkits, err := repository.ListCurrentConfigurationRenameToolkits(context.Background(), 9, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(toolkits) != 2 || toolkits[0].ToolkitID != 2 || toolkits[1].ToolkitID != 5 ||
		!bytes.Equal(toolkits[0].Settings, first) {
		t.Fatalf("toolkits=%#v", toolkits)
	}
	if queries.listParams != (sqlcgen.ListCurrentConfigurationRenameToolkitsParams{
		MaxSettingsBytes: 1024, MaxTotalBytes: 2048, LimitRows: 3,
	}) {
		t.Fatalf("params=%+v", queries.listParams)
	}
	assertCurrentConfigurationLifecycleProjectTx(t, projects, 9, pgx.ReadOnly)
	first[0] = '!'
	if toolkits[0].Settings[0] != '{' {
		t.Fatal("returned settings alias the generated query row")
	}

	queries.listRows = []sqlcgen.ListCurrentConfigurationRenameToolkitsRow{{
		ID: 2, SettingsVersion: currentConfigurationLifecycleEffectsVersion,
		SettingsBytes: 1025, TotalBytes: 1025,
	}}
	if _, err := repository.ListCurrentConfigurationRenameToolkits(context.Background(), 9, limits); !errors.Is(err, configurationapp.ErrCurrentConfigurationLifecycleInternalLimit) {
		t.Fatalf("oversized settings error=%v", err)
	}

	queries.listRows = []sqlcgen.ListCurrentConfigurationRenameToolkitsRow{
		{ID: 2, SettingsVersion: currentConfigurationLifecycleEffectsVersion, Settings: []byte(`{}`), SettingsBytes: 2, TotalBytes: 2},
		{ID: 2, SettingsVersion: currentConfigurationLifecycleEffectsVersion, Settings: []byte(`{}`), SettingsBytes: 2, TotalBytes: 4},
	}
	if _, err := repository.ListCurrentConfigurationRenameToolkits(context.Background(), 9, limits); !errors.Is(err, configurationapp.ErrCurrentConfigurationLifecycleInternalUnavailable) {
		t.Fatalf("unordered row error=%v", err)
	}
}

func TestCurrentConfigurationLifecycleEffectsRenameGetAndCAS(t *testing.T) {
	settings := []byte(`{"elitea_title":"after","private":true}`)
	queries := &currentConfigurationLifecycleEffectsProjectQueriesStub{
		getRow: sqlcgen.GetCurrentConfigurationRenameToolkitRow{
			ID: 17, SettingsVersion: currentConfigurationLifecycleEffectsVersion,
			Settings: settings, SettingsBytes: int64(len(settings)),
		},
		casRows: 1,
	}
	projects := &currentConfigurationLifecycleEffectsProjectStore{}
	repository := mustCurrentConfigurationLifecycleEffectsRepository(t, projects, queries, &currentConfigurationLifecycleEffectsSharedQueriesStub{})

	toolkit, found, err := repository.GetCurrentConfigurationRenameToolkit(context.Background(), 7, 17)
	if err != nil || !found || toolkit.ToolkitID != 17 || !bytes.Equal(toolkit.Settings, settings) {
		t.Fatalf("toolkit=%#v found=%t error=%v", toolkit, found, err)
	}
	if queries.getParams.ToolkitID != 17 || queries.getParams.MaxSettingsBytes != configurationapp.MaxCurrentConfigurationRenameSettingsBytes {
		t.Fatalf("get params=%+v", queries.getParams)
	}
	settings[0] = '!'
	if toolkit.Settings[0] != '{' {
		t.Fatal("returned get settings alias the generated query row")
	}

	updatedSettings := []byte(`{"elitea_title":"new","private":true}`)
	updated, err := repository.CompareAndSwapCurrentConfigurationRenameToolkit(
		context.Background(),
		configurationapp.CurrentConfigurationRenameToolkitUpdate{
			ProjectID: 7, ToolkitID: 17, ExpectedVersion: currentConfigurationLifecycleEffectsVersion,
			Settings: updatedSettings,
		},
	)
	if err != nil || !updated {
		t.Fatalf("updated=%t error=%v", updated, err)
	}
	if queries.casParams.ToolkitID != 17 || queries.casParams.ExpectedVersion != currentConfigurationLifecycleEffectsVersion ||
		!bytes.Equal(queries.casParams.Settings, updatedSettings) {
		t.Fatalf("cas params=%+v", queries.casParams)
	}
	assertCurrentConfigurationLifecycleProjectTx(t, projects, 7, pgx.ReadWrite)

	queries.casRows = 0
	updated, err = repository.CompareAndSwapCurrentConfigurationRenameToolkit(
		context.Background(),
		configurationapp.CurrentConfigurationRenameToolkitUpdate{
			ProjectID: 7, ToolkitID: 17, ExpectedVersion: currentConfigurationLifecycleEffectsVersion,
			Settings: updatedSettings,
		},
	)
	if err != nil || updated {
		t.Fatalf("conflict updated=%t error=%v", updated, err)
	}

	queries.getErr = pgx.ErrNoRows
	_, found, err = repository.GetCurrentConfigurationRenameToolkit(context.Background(), 7, 99)
	if err != nil || found {
		t.Fatalf("missing found=%t error=%v", found, err)
	}
}

func TestCurrentConfigurationLifecycleEffectsListsOnlyBoundedActiveProjects(t *testing.T) {
	shared := &currentConfigurationLifecycleEffectsSharedQueriesStub{projectIDs: []int32{1, 7, 12}}
	repository := mustCurrentConfigurationLifecycleEffectsRepository(
		t, &currentConfigurationLifecycleEffectsProjectStore{}, &currentConfigurationLifecycleEffectsProjectQueriesStub{}, shared,
	)

	projectIDs, err := repository.ListActiveCurrentProjectIDs(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if shared.projectLimit != 4 || len(projectIDs) != 3 || projectIDs[2] != 12 {
		t.Fatalf("limit=%d project IDs=%v", shared.projectLimit, projectIDs)
	}
	shared.projectIDs[0] = 99
	if projectIDs[0] != 1 {
		t.Fatal("returned project IDs alias the generated query result")
	}

	shared.projectIDs = []int32{7, 7}
	if _, err := repository.ListActiveCurrentProjectIDs(context.Background(), 2); !errors.Is(err, configurationapp.ErrCurrentConfigurationLifecycleInternalUnavailable) {
		t.Fatalf("duplicate project error=%v", err)
	}
}

func TestCurrentConfigurationLifecycleEffectsApplicationReplacementIsAtomicAndBounded(t *testing.T) {
	queries := &currentConfigurationLifecycleEffectsProjectQueriesStub{
		replaceRow: sqlcgen.ReplaceCurrentDeletedLLMApplicationReferencesRow{MatchedCount: 3, UpdatedCount: 3},
	}
	projects := &currentConfigurationLifecycleEffectsProjectStore{}
	repository := mustCurrentConfigurationLifecycleEffectsRepository(t, projects, queries, &currentConfigurationLifecycleEffectsSharedQueriesStub{})
	replacement := configurationapp.CurrentDeletedLLMReferenceReplacement{
		ProjectID: 7, DeletedModelName: "old-model", DefaultModelName: "default-model",
		DefaultModelProjectID: 1, MaxRows: 3,
	}

	updated, err := repository.ReplaceCurrentDeletedLLMApplicationReferences(context.Background(), replacement)
	if err != nil || updated != 3 {
		t.Fatalf("updated=%d error=%v", updated, err)
	}
	if queries.replaceParams != (sqlcgen.ReplaceCurrentDeletedLLMApplicationReferencesParams{
		DeletedModelName: "old-model", ScanLimit: 4, MaxRows: 3,
		DefaultModelName: "default-model", DefaultModelProjectID: 1,
	}) {
		t.Fatalf("params=%+v", queries.replaceParams)
	}
	assertCurrentConfigurationLifecycleProjectTx(t, projects, 7, pgx.ReadWrite)

	queries.replaceRow = sqlcgen.ReplaceCurrentDeletedLLMApplicationReferencesRow{MatchedCount: 4, UpdatedCount: 0}
	if _, err := repository.ReplaceCurrentDeletedLLMApplicationReferences(context.Background(), replacement); !errors.Is(err, configurationapp.ErrCurrentConfigurationLifecycleInternalLimit) {
		t.Fatalf("overflow error=%v", err)
	}

	queries.replaceRow = sqlcgen.ReplaceCurrentDeletedLLMApplicationReferencesRow{MatchedCount: 3, UpdatedCount: 2}
	if _, err := repository.ReplaceCurrentDeletedLLMApplicationReferences(context.Background(), replacement); !errors.Is(err, configurationapp.ErrCurrentConfigurationLifecycleInternalUnavailable) {
		t.Fatalf("partial update error=%v", err)
	}
}

func TestCurrentConfigurationLifecycleEffectsRejectsInvalidRequestsAndPreservesCancellation(t *testing.T) {
	queries := &currentConfigurationLifecycleEffectsProjectQueriesStub{}
	shared := &currentConfigurationLifecycleEffectsSharedQueriesStub{}
	repository := mustCurrentConfigurationLifecycleEffectsRepository(t, &currentConfigurationLifecycleEffectsProjectStore{}, queries, shared)

	if _, err := NewCurrentConfigurationLifecycleEffectsRepository(nil); err == nil {
		t.Fatal("expected nil pool rejection")
	}
	if _, err := repository.SetCurrentConfigurationLifecycleStatus(context.Background(), configurationapp.CurrentConfigurationLifecycleStatusTarget{}); !errors.Is(err, configurationapp.ErrInvalidCurrentConfigurationLifecycleInternalEffect) {
		t.Fatalf("invalid status error=%v", err)
	}
	if _, err := repository.ListCurrentConfigurationRenameToolkits(context.Background(), 7, configurationapp.CurrentConfigurationRenameScanLimits{}); !errors.Is(err, configurationapp.ErrInvalidCurrentConfigurationLifecycleInternalEffect) {
		t.Fatalf("invalid scan error=%v", err)
	}
	if _, err := repository.ListActiveCurrentProjectIDs(context.Background(), configurationapp.MaxCurrentDeletedLLMProjects+2); !errors.Is(err, configurationapp.ErrInvalidCurrentConfigurationLifecycleInternalEffect) {
		t.Fatalf("invalid project list error=%v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	before := queries.statusCalls
	_, err := repository.SetCurrentConfigurationLifecycleStatus(canceled, configurationapp.CurrentConfigurationLifecycleStatusTarget{
		ProjectID: 7, ConfigurationID: 1,
		ConfigurationUUID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
	})
	if !errors.Is(err, context.Canceled) || queries.statusCalls != before {
		t.Fatalf("canceled error=%v calls=%d", err, queries.statusCalls)
	}

	queries.statusErr = errors.New("password=TEST_ONLY_DO_NOT_EMIT")
	_, err = repository.SetCurrentConfigurationLifecycleStatus(context.Background(), configurationapp.CurrentConfigurationLifecycleStatusTarget{
		ProjectID: 7, ConfigurationID: 1,
		ConfigurationUUID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
	})
	if !errors.Is(err, configurationapp.ErrCurrentConfigurationLifecycleInternalUnavailable) || strings.Contains(err.Error(), "password") {
		t.Fatalf("storage error=%v", err)
	}
}

func TestCurrentConfigurationLifecycleEffectsSQLHasBoundsFencesAndAtomicGate(t *testing.T) {
	sourceBytes, err := os.ReadFile("../../../db/queries/configuration_lifecycle_effects.sql")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	fragments := []string{
		"AND id = sqlc.arg('configuration_id')::integer",
		"AND uuid = sqlc.arg('configuration_uuid')::text::uuid",
		"ORDER BY toolkit.id",
		"LIMIT sqlc.arg('limit_rows')::integer",
		"octet_length(settings::text)",
		"ELSE NULL::jsonb",
		"md5(settings::text)",
		"AND md5(settings::text) = sqlc.arg('expected_version')::text",
		"project.create_success IS TRUE",
		"project.suspended IS FALSE",
		"LIMIT sqlc.arg('scan_limit')::integer",
		"FOR UPDATE",
		"count(*) FROM matched) <= sqlc.arg('max_rows')::integer",
		"version.llm_settings::jsonb || jsonb_build_object",
	}
	for _, fragment := range fragments {
		if !strings.Contains(source, fragment) {
			t.Fatalf("lifecycle effects SQL is missing %q", fragment)
		}
	}
}

func mustCurrentConfigurationLifecycleEffectsRepository(
	t *testing.T,
	projects projectStore,
	queries *currentConfigurationLifecycleEffectsProjectQueriesStub,
	shared currentConfigurationLifecycleEffectsSharedQueries,
) *CurrentConfigurationLifecycleEffectsRepository {
	t.Helper()
	repository, err := newCurrentConfigurationLifecycleEffectsRepository(
		projects,
		func(sqlExecutor) (currentConfigurationLifecycleEffectsProjectQueries, error) { return queries, nil },
		shared,
	)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func assertCurrentConfigurationLifecycleProjectTx(
	t *testing.T,
	store *currentConfigurationLifecycleEffectsProjectStore,
	projectID int64,
	accessMode pgx.TxAccessMode,
) {
	t.Helper()
	if store.projectID != projectID || store.options.IsoLevel != pgx.ReadCommitted || store.options.AccessMode != accessMode {
		t.Fatalf("project=%d options=%+v", store.projectID, store.options)
	}
}

type currentConfigurationLifecycleEffectsProjectStore struct {
	projectID int64
	options   pgx.TxOptions
	err       error
}

func (s *currentConfigurationLifecycleEffectsProjectStore) WithinProjectTx(
	_ context.Context,
	projectID int64,
	options pgx.TxOptions,
	fn func(sqlExecutor) error,
) error {
	s.projectID = projectID
	s.options = options
	if s.err != nil {
		return s.err
	}
	return fn(nil)
}

type currentConfigurationLifecycleEffectsProjectQueriesStub struct {
	statusParams sqlcgen.SetCurrentConfigurationLifecycleStatusParams
	statusRows   int64
	statusErr    error
	statusCalls  int

	listParams sqlcgen.ListCurrentConfigurationRenameToolkitsParams
	listRows   []sqlcgen.ListCurrentConfigurationRenameToolkitsRow
	listErr    error

	getParams sqlcgen.GetCurrentConfigurationRenameToolkitParams
	getRow    sqlcgen.GetCurrentConfigurationRenameToolkitRow
	getErr    error

	casParams sqlcgen.CompareAndSwapCurrentConfigurationRenameToolkitParams
	casRows   int64
	casErr    error

	replaceParams sqlcgen.ReplaceCurrentDeletedLLMApplicationReferencesParams
	replaceRow    sqlcgen.ReplaceCurrentDeletedLLMApplicationReferencesRow
	replaceErr    error
}

func (s *currentConfigurationLifecycleEffectsProjectQueriesStub) SetCurrentConfigurationLifecycleStatus(
	_ context.Context,
	params sqlcgen.SetCurrentConfigurationLifecycleStatusParams,
) (int64, error) {
	s.statusCalls++
	s.statusParams = params
	return s.statusRows, s.statusErr
}

func (s *currentConfigurationLifecycleEffectsProjectQueriesStub) ListCurrentConfigurationRenameToolkits(
	_ context.Context,
	params sqlcgen.ListCurrentConfigurationRenameToolkitsParams,
) ([]sqlcgen.ListCurrentConfigurationRenameToolkitsRow, error) {
	s.listParams = params
	return s.listRows, s.listErr
}

func (s *currentConfigurationLifecycleEffectsProjectQueriesStub) GetCurrentConfigurationRenameToolkit(
	_ context.Context,
	params sqlcgen.GetCurrentConfigurationRenameToolkitParams,
) (sqlcgen.GetCurrentConfigurationRenameToolkitRow, error) {
	s.getParams = params
	return s.getRow, s.getErr
}

func (s *currentConfigurationLifecycleEffectsProjectQueriesStub) CompareAndSwapCurrentConfigurationRenameToolkit(
	_ context.Context,
	params sqlcgen.CompareAndSwapCurrentConfigurationRenameToolkitParams,
) (int64, error) {
	s.casParams = params
	s.casParams.Settings = append([]byte(nil), params.Settings...)
	return s.casRows, s.casErr
}

func (s *currentConfigurationLifecycleEffectsProjectQueriesStub) ReplaceCurrentDeletedLLMApplicationReferences(
	_ context.Context,
	params sqlcgen.ReplaceCurrentDeletedLLMApplicationReferencesParams,
) (sqlcgen.ReplaceCurrentDeletedLLMApplicationReferencesRow, error) {
	s.replaceParams = params
	return s.replaceRow, s.replaceErr
}

type currentConfigurationLifecycleEffectsSharedQueriesStub struct {
	projectLimit int32
	projectIDs   []int32
	err          error
}

func (s *currentConfigurationLifecycleEffectsSharedQueriesStub) ListActiveCurrentProjectIDs(
	_ context.Context,
	limit int32,
) ([]int32, error) {
	s.projectLimit = limit
	return s.projectIDs, s.err
}
