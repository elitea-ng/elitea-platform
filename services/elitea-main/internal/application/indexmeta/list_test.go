package indexmeta

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
)

func TestServiceListPreservesCurrentIndexMetaContract(t *testing.T) {
	t.Parallel()

	now := time.Unix(20_000, 500_000_000)
	toolkits := &currentToolkitStub{toolkit: indexingapp.CurrentToolkitSnapshot{
		ID: 42, Type: "sample_toolkit", Settings: map[string]any{
			"pgvector_configuration": map[string]any{"elitea_title": "elitea-pgvector", "private": true},
			"github_configuration":   map[string]any{"elitea_title": "must-not-materialize", "private": true},
		},
	}, found: true}
	settings := &currentSettingsStub{result: map[string]any{
		"pgvector_configuration": map[string]any{"connection_string": "postgresql+psycopg://secret-canary@pg/project"},
	}}
	timeouts := &currentTimeoutStub{timeout: 2 * time.Hour}
	reader := &currentReaderStub{records: []RawRecord{
		{ID: "recent", Metadata: json.RawMessage(`{"state":"in_progress","updated_on":19990}`)},
		{ID: "old", Metadata: json.RawMessage(`{"state":"in_progress","updated_on":10000,"index_configuration":"{\"full\":true}","history":"[{\"state\":\"completed\",\"timestamp\":1}]"}`)},
		{ID: "done", Metadata: json.RawMessage(`{"state":"completed","updated_on":0}`)},
	}}
	service := currentServiceForTest(t, toolkits, settings, timeouts, reader, func() time.Time { return now })

	items, err := service.List(context.Background(), Request{ProjectID: 7, ActorUserID: 8, ToolkitID: 42})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(items) != 3 || items[0].ID != "recent" || items[0].Stale || !items[1].Stale || items[2].Stale {
		t.Fatalf("List() items = %+v", items)
	}
	configuration, ok := items[1].Metadata["index_configuration"].(map[string]any)
	if !ok || configuration["full"] != true {
		t.Fatalf("decoded index_configuration = %#v", items[1].Metadata["index_configuration"])
	}
	history, ok := items[1].Metadata["history"].([]any)
	if !ok || history[0].(map[string]any)["state"] != "created" {
		t.Fatalf("normalized history = %#v", items[1].Metadata["history"])
	}

	if toolkits.projectID != 7 || toolkits.userID != 8 || toolkits.toolkitID != 42 {
		t.Fatalf("toolkit lookup = project %d user %d toolkit %d", toolkits.projectID, toolkits.userID, toolkits.toolkitID)
	}
	if settings.request.Mode != configurationapp.CurrentToolkitSettingsClaimMode ||
		settings.request.ProjectID != 7 || settings.request.UserID != 8 || settings.request.ToolkitType != "sample_toolkit" ||
		len(settings.request.Settings) != 1 || settings.request.Settings["pgvector_configuration"] == nil {
		t.Fatalf("settings request = %+v", settings.request)
	}
	if timeouts.projectID != 7 {
		t.Fatalf("timeout project = %d", timeouts.projectID)
	}
	if reader.target.ConnectionString != "postgresql+psycopg://secret-canary@pg/project" ||
		reader.target.SchemaID != 42 || reader.target.MaxRows != MaxCurrentIndexMetaRows ||
		reader.target.MaxMetadataBytes != MaxCurrentIndexMetaMetadataBytes ||
		reader.target.MaxTotalBytes != MaxCurrentIndexMetaTotalBytes {
		t.Fatalf("resolved target = %+v", reader.target)
	}

	encoded, err := json.Marshal(items[:0])
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "[]" {
		t.Fatalf("empty response JSON = %s", encoded)
	}
}

// elitea_issues: #5867 — Toolkit Index Reindexation History must show one
// row per (re)indexation operation, not two. The dual-writer bootstrap
// (Main + SDK writing adjacent in_progress/completed entries for the same
// run) is collapsed here to a single "created" entry in the served history.
func TestServiceListProjectsExistingDualWriterBootstrapAsCreated(t *testing.T) {
	t.Parallel()

	toolkits := &currentToolkitStub{toolkit: indexingapp.CurrentToolkitSnapshot{
		ID:   42,
		Type: "sample_toolkit",
		Settings: map[string]any{
			"pgvector_configuration": map[string]any{
				"elitea_title": "elitea-pgvector",
				"private":      true,
			},
		},
	}, found: true}
	settings := &currentSettingsStub{result: map[string]any{
		"pgvector_configuration": map[string]any{
			"connection_string": "postgresql+psycopg://pg/project",
		},
	}}
	historyRaw, err := json.Marshal([]map[string]any{
		{
			"state":                "in_progress",
			"indexed":              0,
			"updated":              0,
			"index_meta_id":        "meta-15",
			"execution_id":         "execution-15",
			"execution_generation": 1,
			"index_generation":     15,
		},
		{
			"state":                "completed",
			"indexed":              61,
			"updated":              228,
			"index_meta_id":        "meta-15",
			"execution_id":         "execution-15",
			"execution_generation": 1,
			"index_generation":     15,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := json.Marshal(map[string]any{
		"state":      "completed",
		"updated_on": 20_000,
		"history":    string(historyRaw),
	})
	if err != nil {
		t.Fatal(err)
	}
	reader := &currentReaderStub{records: []RawRecord{{
		ID:       "dual-writer",
		Metadata: metadata,
	}}}
	service := currentServiceForTest(
		t,
		toolkits,
		settings,
		&currentTimeoutStub{timeout: 2 * time.Hour},
		reader,
		func() time.Time { return time.Unix(20_000, 0) },
	)

	items, err := service.List(
		context.Background(),
		Request{ProjectID: 7, ActorUserID: 8, ToolkitID: 42},
	)
	if err != nil {
		t.Fatal(err)
	}
	history, ok := items[0].Metadata["history"].([]any)
	if !ok || len(history) != 2 {
		t.Fatalf("history=%#v", items[0].Metadata["history"])
	}
	first, firstOK := history[0].(map[string]any)
	second, secondOK := history[1].(map[string]any)
	if !firstOK || !secondOK ||
		first["state"] != "created" ||
		second["state"] != "completed" ||
		second["indexed"] != json.Number("61") ||
		second["updated"] != json.Number("228") {
		t.Fatalf("history=%#v", history)
	}
}

func TestDualWriterProjectionRequiresZeroCountExactRunIdentity(t *testing.T) {
	t.Parallel()

	baseFirst := map[string]any{
		"state":                "in_progress",
		"indexed":              json.Number("0"),
		"updated":              json.Number("0"),
		"index_meta_id":        "meta-15",
		"execution_id":         "execution-15",
		"execution_generation": json.Number("1"),
		"index_generation":     json.Number("15"),
	}
	baseSecond := map[string]any{
		"state":                "completed",
		"indexed":              json.Number("61"),
		"updated":              json.Number("228"),
		"index_meta_id":        "meta-15",
		"execution_id":         "execution-15",
		"execution_generation": json.Number("1"),
		"index_generation":     json.Number("15"),
	}
	for _, test := range []struct {
		name string
		edit func(map[string]any, map[string]any)
	}{
		{
			name: "nonzero bootstrap",
			edit: func(first, _ map[string]any) {
				first["indexed"] = json.Number("1")
			},
		},
		{
			name: "different generation",
			edit: func(_, second map[string]any) {
				second["index_generation"] = json.Number("16")
			},
		},
		{
			name: "missing identity",
			edit: func(first, _ map[string]any) {
				delete(first, "index_meta_id")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			first := cloneCurrentHistoryForTest(baseFirst)
			second := cloneCurrentHistoryForTest(baseSecond)
			test.edit(first, second)
			metadata := map[string]any{"history": []any{first, second}}
			markCurrentFirstHistoryEntryCreated(metadata)
			if first["state"] != "in_progress" {
				t.Fatalf("history=%#v", metadata["history"])
			}
		})
	}
}

func TestServiceListRejectsRequestAndResolvedTargetFailures(t *testing.T) {
	t.Parallel()

	validToolkit := indexingapp.CurrentToolkitSnapshot{ID: 3, Type: "sample_toolkit", Settings: map[string]any{
		"pgvector_configuration": map[string]any{"elitea_title": "elitea-pgvector", "private": true},
	}}
	tests := []struct {
		name     string
		request  Request
		toolkits *currentToolkitStub
		settings *currentSettingsStub
		want     error
	}{
		{
			name: "invalid identity", request: Request{}, toolkits: &currentToolkitStub{}, settings: &currentSettingsStub{},
			want: ErrInvalidCurrentIndexMetaRequest,
		},
		{
			name: "toolkit invisible", request: Request{ProjectID: 1, ActorUserID: 2, ToolkitID: 3},
			toolkits: &currentToolkitStub{}, settings: &currentSettingsStub{}, want: ErrCurrentIndexMetaToolkitMissing,
		},
		{
			name: "toolkit ID mismatch", request: Request{ProjectID: 1, ActorUserID: 2, ToolkitID: 3},
			toolkits: &currentToolkitStub{toolkit: indexingapp.CurrentToolkitSnapshot{ID: 4, Type: "sample_toolkit", Settings: map[string]any{}}, found: true},
			settings: &currentSettingsStub{}, want: ErrCurrentIndexMetaTargetMissing,
		},
		{
			name: "configuration missing", request: Request{ProjectID: 1, ActorUserID: 2, ToolkitID: 3},
			toolkits: &currentToolkitStub{toolkit: validToolkit, found: true}, settings: &currentSettingsStub{result: map[string]any{}},
			want: ErrCurrentIndexMetaTargetMissing,
		},
		{
			name: "configuration DSN bounded", request: Request{ProjectID: 1, ActorUserID: 2, ToolkitID: 3},
			toolkits: &currentToolkitStub{toolkit: validToolkit, found: true}, settings: &currentSettingsStub{result: map[string]any{
				"pgvector_configuration": map[string]any{"connection_string": strings.Repeat("x", MaxCurrentPgvectorDSNBytes+1)},
			}}, want: ErrCurrentIndexMetaTargetMissing,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service := currentServiceForTest(
				t,
				test.toolkits,
				test.settings,
				&currentTimeoutStub{timeout: 2 * time.Hour},
				&currentReaderStub{},
				time.Now,
			)
			_, err := service.List(context.Background(), test.request)
			if !errors.Is(err, test.want) {
				t.Fatalf("List() error = %v, want %v", err, test.want)
			}
		})
	}
}

func cloneCurrentHistoryForTest(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func TestServiceListFailsClosedWithoutLeakingDependencies(t *testing.T) {
	t.Parallel()

	secret := "postgresql://user:secret-canary@host/database"
	validToolkit := &currentToolkitStub{toolkit: indexingapp.CurrentToolkitSnapshot{
		ID: 3, Type: "sample_toolkit", Settings: map[string]any{
			"pgvector_configuration": map[string]any{"elitea_title": "elitea-pgvector", "private": true},
		},
	}, found: true}
	settings := &currentSettingsStub{result: map[string]any{
		"pgvector_configuration": map[string]any{"connection_string": secret},
	}}

	t.Run("reader error is redacted", func(t *testing.T) {
		service := currentServiceForTest(t, validToolkit, settings, &currentTimeoutStub{timeout: time.Hour}, &currentReaderStub{
			err: errors.New("driver exposed " + secret),
		}, time.Now)
		_, err := service.List(context.Background(), Request{ProjectID: 1, ActorUserID: 2, ToolkitID: 3})
		if !errors.Is(err, ErrCurrentIndexMetaUnavailable) || strings.Contains(err.Error(), "secret-canary") {
			t.Fatalf("List() error = %v", err)
		}
	})

	t.Run("malformed metadata fails atomically", func(t *testing.T) {
		service := currentServiceForTest(t, validToolkit, settings, &currentTimeoutStub{timeout: time.Hour}, &currentReaderStub{
			records: []RawRecord{{ID: "valid", Metadata: json.RawMessage(`{}`)}, {ID: "invalid", Metadata: json.RawMessage(`[]`)}},
		}, time.Now)
		items, err := service.List(context.Background(), Request{ProjectID: 1, ActorUserID: 2, ToolkitID: 3})
		if items != nil || !errors.Is(err, ErrCurrentIndexMetaInvalid) {
			t.Fatalf("List() = %+v, %v", items, err)
		}
	})

	t.Run("excess rows fail instead of truncating", func(t *testing.T) {
		records := make([]RawRecord, MaxCurrentIndexMetaRows+1)
		service := currentServiceForTest(t, validToolkit, settings, &currentTimeoutStub{timeout: time.Hour}, &currentReaderStub{records: records}, time.Now)
		_, err := service.List(context.Background(), Request{ProjectID: 1, ActorUserID: 2, ToolkitID: 3})
		if !errors.Is(err, ErrCurrentIndexMetaLimitExceeded) {
			t.Fatalf("List() error = %v", err)
		}
	})

	t.Run("reader limit remains distinguishable", func(t *testing.T) {
		service := currentServiceForTest(t, validToolkit, settings, &currentTimeoutStub{timeout: time.Hour}, &currentReaderStub{
			err: ErrCurrentIndexMetaLimitExceeded,
		}, time.Now)
		_, err := service.List(context.Background(), Request{ProjectID: 1, ActorUserID: 2, ToolkitID: 3})
		if !errors.Is(err, ErrCurrentIndexMetaLimitExceeded) {
			t.Fatalf("List() error = %v", err)
		}
	})
}

func TestServiceListPreservesCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service := currentServiceForTest(t, &currentToolkitStub{}, &currentSettingsStub{}, &currentTimeoutStub{}, &currentReaderStub{}, time.Now)
	_, err := service.List(ctx, Request{ProjectID: 1, ActorUserID: 2, ToolkitID: 3})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("List() error = %v", err)
	}

	toolkits := &currentToolkitStub{toolkit: indexingapp.CurrentToolkitSnapshot{ID: 3, Type: "sample_toolkit", Settings: map[string]any{
		"pgvector_configuration": map[string]any{"elitea_title": "elitea-pgvector", "private": true},
	}}, found: true}
	settings := &currentSettingsStub{result: map[string]any{
		"pgvector_configuration": map[string]any{"connection_string": "postgresql://project"},
	}}
	service = currentServiceForTest(t, toolkits, settings, &currentTimeoutStub{timeout: time.Hour}, &currentReaderStub{err: context.DeadlineExceeded}, time.Now)
	_, err = service.List(context.Background(), Request{ProjectID: 1, ActorUserID: 2, ToolkitID: 3})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("List() error = %v", err)
	}
}

func currentServiceForTest(
	t *testing.T,
	toolkits indexingapp.CurrentToolkitReader,
	settings indexingapp.CurrentToolkitSettingsValidator,
	timeouts StaleTimeoutResolver,
	reader ExternalReader,
	now func() time.Time,
) *Service {
	t.Helper()
	service, err := newService(toolkits, settings, timeouts, reader, now)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

type currentToolkitStub struct {
	toolkit                      indexingapp.CurrentToolkitSnapshot
	found                        bool
	err                          error
	projectID, userID, toolkitID int32
}

func (s *currentToolkitStub) GetCurrentToolkit(
	_ context.Context,
	projectID, userID, toolkitID int32,
) (indexingapp.CurrentToolkitSnapshot, bool, error) {
	s.projectID, s.userID, s.toolkitID = projectID, userID, toolkitID
	return s.toolkit, s.found, s.err
}

type currentSettingsStub struct {
	request configurationapp.CurrentToolkitSettingsRequest
	result  map[string]any
	err     error
}

func (s *currentSettingsStub) Resolve(
	_ context.Context,
	request configurationapp.CurrentToolkitSettingsRequest,
) (map[string]any, error) {
	s.request = request
	return s.result, s.err
}

type currentTimeoutStub struct {
	projectID int32
	timeout   time.Duration
	err       error
}

func (s *currentTimeoutStub) ResolveCurrentIndexMetaStaleTimeout(_ context.Context, projectID int32) (time.Duration, error) {
	s.projectID = projectID
	return s.timeout, s.err
}

type currentReaderStub struct {
	target  ResolvedTarget
	records []RawRecord
	err     error
}

func (s *currentReaderStub) List(_ context.Context, target ResolvedTarget) ([]RawRecord, error) {
	s.target = target
	return s.records, s.err
}
