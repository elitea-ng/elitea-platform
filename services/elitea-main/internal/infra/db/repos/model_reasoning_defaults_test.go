package repos

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// Every seed of shared 0225 must compile in Go RE2, or Main skips it.
func TestEveryModelReasoningSeedCompiles(t *testing.T) {
	raw, err := platformmigrations.Files.ReadFile("shared/0225_model_reasoning_control.sql")
	if err != nil {
		t.Fatal(err)
	}
	seeds := regexp.MustCompile(`\('([^']+)',\s*'([a-z_]+)',\s*(TRUE|FALSE),`).FindAllStringSubmatch(string(raw), -1)
	if len(seeds) < 5 {
		t.Fatalf("found %d seeds, want the family rows", len(seeds))
	}
	var rules []configurationapp.ModelReasoningDefault
	for _, seed := range seeds {
		rule, err := configurationapp.CompileModelReasoningDefault(seed[1], seed[2], seed[3] == "TRUE")
		if err != nil {
			t.Fatalf("seed %q: %v", seed[1], err)
		}
		rules = append(rules, rule)
	}

	// The seeds in file order are the priority order of the table.
	yes := true
	for name, want := range map[string]string{
		"Qwen/Qwen3-8B":                      "toggle",
		"qwen3-30b-a3b":                      "toggle",
		"Qwen/Qwen3-235B-A22B-Thinking-2507": "fixed_on",
		"QwQ-32B":                            "fixed_on",
		"deepseek-reasoner":                  "fixed_on",
		"gpt-5.1":                            "effort",
		"openai/gpt-5.2-pro":                 "effort",
		"gpt-5-mini":                         "",
		"openai/o3-mini":                     "",
		"o4-mini":                            "",
		"claude-sonnet-4-5":                  "budget",
		"us.anthropic.claude-opus-4-1":       "budget",
		"gemini-2.5-flash":                   "budget",
		"gemini-2.5-pro":                     "fixed_on",
		"gemini-3-flash":                     "",
		"gpt-4o":                             "",
		"omni-moderation":                    "",
	} {
		item := configurationapp.CurrentModelCatalogItem{Name: name, SupportsReasoning: &yes}
		configurationapp.ApplyModelReasoningDefaults(&item, rules)
		got := ""
		if item.ReasoningControl != nil {
			got = *item.ReasoningControl
		}
		if got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
}

type fakeReasoningRows struct {
	rows  [][3]any
	index int
}

func (r *fakeReasoningRows) Close()     {}
func (r *fakeReasoningRows) Err() error { return nil }
func (r *fakeReasoningRows) Next() bool { r.index++; return r.index <= len(r.rows) }
func (r *fakeReasoningRows) Scan(dest ...any) error {
	row := r.rows[r.index-1]
	*dest[0].(*string) = row[0].(string)
	*dest[1].(*string) = row[1].(string)
	*dest[2].(*bool) = row[2].(bool)
	return nil
}

type fakeReasoningQuerier struct {
	calls int
	err   error
	rows  [][3]any
}

func (q *fakeReasoningQuerier) Query(context.Context, string, ...any) (sqlRows, error) {
	q.calls++
	if q.err != nil {
		return nil, q.err
	}
	return &fakeReasoningRows{rows: q.rows}, nil
}

func TestModelReasoningDefaultsReaderCachesAndNeverFails(t *testing.T) {
	db := &fakeReasoningQuerier{rows: [][3]any{
		{"(?<=bad)", "toggle", false}, // RE2 refuses it: skipped, not fatal
		{"(^|/)qwen", "toggle", false},
	}}
	clock := time.Unix(1_700_000_000, 0)
	reader := &modelReasoningDefaultsReader{db: db, now: func() time.Time { return clock }}

	rules := reader.Load(context.Background())
	if len(rules) != 1 || db.calls != 1 {
		t.Fatalf("rules=%d calls=%d, want the one valid row from one read", len(rules), db.calls)
	}
	reader.Load(context.Background())
	if db.calls != 1 {
		t.Fatal("a second read inside the TTL must use the cache")
	}

	// A failed refresh keeps the last rows.
	clock = clock.Add(modelReasoningDefaultsTTL + time.Second)
	db.err = errors.New("connection reset")
	if got := reader.Load(context.Background()); len(got) != 1 {
		t.Fatalf("a failed refresh must keep the last rows, got %d", len(got))
	}

	// A database before 0225 has no table: no defaults, no failure.
	missing := &modelReasoningDefaultsReader{
		db:  &fakeReasoningQuerier{err: &pgconn.PgError{Code: "42P01"}},
		now: time.Now,
	}
	if got := missing.Load(context.Background()); got != nil {
		t.Fatalf("a missing table must read as no defaults, got %v", got)
	}
}
