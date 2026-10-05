package repos

import (
	"context"
	"reflect"
	"testing"
	"time"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

// The two reads the platform default model release takes (#6826), against a
// real PostgreSQL: the keyset page of active projects, and whether another row
// still serves a model name.
func TestPlatformModelDefaultRowsPostgres(t *testing.T) {
	pool := newPostgresIntegrationPool(t)
	prepareCurrentConfigurationLifecycleEffectsPostgres(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.configuration (
    id, uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, source
) VALUES
    (21, '21212121-2121-4121-8121-212121212121', 1, 'GPT Azure', 'gpt-azure', 'llm_model', 'llm',
     '{"name":"gpt-4o"}', '{}', true, true, 'user'),
    (22, '22222222-2222-4222-8222-222222222223', 1, 'GPT OpenAI', 'gpt-openai', 'llm_model', 'llm',
     '{"name":"gpt-4o"}', '{}', true, false, 'user'),
    (23, '23232323-2323-4323-8323-232323232323', 1, 'Scratch', 'scratch', 'llm_model', 'llm',
     '{"name":"scratch"}', '{}', false, true, 'user'),
    (24, '24242424-2424-4424-8424-242424242424', 1, 'Vectors', 'pgvector', 'pgvector', 'vectorstorage',
     '{}', '{}', true, true, 'user');`); err != nil {
		t.Fatalf("seed model rows: %v", err)
	}

	repository, err := NewPlatformModelDefaultRowsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		query configurationapp.PlatformModelDefaultRowQuery
		want  bool
	}{
		{name: "a same-name sibling survives, disabled or not",
			query: configurationapp.PlatformModelDefaultRowQuery{ProjectID: 1, Section: "llm", Name: "gpt-4o", SharedOnly: true, ExcludeID: 21},
			want:  true},
		{name: "the row itself is left out",
			query: configurationapp.PlatformModelDefaultRowQuery{ProjectID: 1, Section: "llm", Name: "scratch", ExcludeID: 23},
			want:  false},
		{name: "a non-shared row is no shared survivor",
			query: configurationapp.PlatformModelDefaultRowQuery{ProjectID: 1, Section: "llm", Name: "scratch", SharedOnly: true},
			want:  false},
		{name: "a non-shared row serves its own project",
			query: configurationapp.PlatformModelDefaultRowQuery{ProjectID: 1, Section: "llm", Name: "scratch"},
			want:  true},
		{name: "another section does not serve the name",
			query: configurationapp.PlatformModelDefaultRowQuery{ProjectID: 1, Section: "embedding", Name: "gpt-4o"},
			want:  false},
		{name: "a vector storage row is named by its title",
			query: configurationapp.PlatformModelDefaultRowQuery{ProjectID: 1, Section: "vectorstorage", Name: "pgvector"},
			want:  true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repository.ModelRowExists(ctx, tc.query)
			if err != nil || got != tc.want {
				t.Fatalf("ModelRowExists(%+v) = %v err=%v, want %v", tc.query, got, err, tc.want)
			}
		})
	}

	// Projects 1 and 4 are active; 2 is incomplete and 3 is suspended.
	first, err := repository.ListActiveProjectIDsAfter(ctx, 0, 1)
	if err != nil || !reflect.DeepEqual(first, []int32{1}) {
		t.Fatalf("first page = %v err=%v", first, err)
	}
	second, err := repository.ListActiveProjectIDsAfter(ctx, 1, 1)
	if err != nil || !reflect.DeepEqual(second, []int32{4}) {
		t.Fatalf("second page = %v err=%v", second, err)
	}
	last, err := repository.ListActiveProjectIDsAfter(ctx, 4, 1)
	if err != nil || len(last) != 0 {
		t.Fatalf("last page = %v err=%v", last, err)
	}
	if _, err := repository.ListActiveProjectIDsAfter(ctx, 0, configurationapp.PlatformModelDefaultProjectPage+1); err == nil {
		t.Fatal("a page above the bound was read")
	}
}
