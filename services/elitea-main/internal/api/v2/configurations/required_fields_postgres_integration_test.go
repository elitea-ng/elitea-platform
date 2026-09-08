package configurations_test

// The CREATE route's required-field contract, against a real PostgreSQL.
//
// The unit test beside this one (required_fields_internal_test.go) proves the
// walk over the pinned schema. This file proves the two things only a real
// request against a real database can: that the refusal is a 400 the caller
// can act on, and that a refused create WROTE NOTHING. A 400 that still
// inserted would leave exactly the row the refusal exists to prevent, and no
// unit test of the validator can see it.
//
// The accepted half is here for the same reason. A rule that refuses
// everything also passes every refusal assertion, so each section's smallest
// LEGAL body is created and read back: the row exists, and it is filed under
// the section its type belongs to.
//
// No body below carries a schema-declared password (`api_key`,
// `connection_string`, `access_token`). This harness composes no project
// vault, and the route refuses a plaintext secret with 503 rather than storing
// it (secret_sealing.go) — a refusal about the vault, in a file about missing
// fields, would read as this rule firing when it did not.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCreateRefusesABodyMissingASchemaRequiredField(t *testing.T) {
	pool := newCreateEchoPool(t)
	router := createEchoRouter(pool)

	cases := []struct {
		name  string
		title string
		body  map[string]any
		// field is the path the refusal must name. A refusal that says only
		// "invalid request" leaves the caller guessing which of five fields it
		// was, which is the state this rule replaces.
		field string
	}{
		{
			name:  "an Azure OpenAI credential with no endpoint",
			title: "autotest_azure_no_endpoint",
			body: map[string]any{
				"elitea_title": "autotest_azure_no_endpoint",
				"label":        "autotest azure",
				"type":         "azure_open_ai",
				"data":         map[string]any{"api_version": "2024-02-01"},
			},
			field: "data.api_base",
		},
		{
			name:  "a vector store with no label and no data",
			title: "autotest_pgvector_incomplete",
			body: map[string]any{
				"elitea_title": "autotest_pgvector_incomplete",
				"type":         "pgvector",
			},
			field: "label",
		},
		{
			name:  "a toolkit credential with no label and no data",
			title: "autotest_github_incomplete",
			body: map[string]any{
				"elitea_title": "autotest_github_incomplete",
				"type":         "github",
			},
			field: "data",
		},
		{
			name:  "an embedding model with no model name",
			title: "autotest_embedding_no_name",
			body: map[string]any{
				"elitea_title": "autotest_embedding_no_name",
				"label":        "autotest embedding",
				"type":         "embedding_model",
				"data": map[string]any{
					"ai_credentials": map[string]any{"elitea_title": "autotest_cred", "private": false},
				},
			},
			field: "data.name",
		},
		{
			name:  "an embedding model with no credential link",
			title: "autotest_embedding_no_link",
			body: map[string]any{
				"elitea_title": "autotest_embedding_no_link",
				"label":        "autotest embedding",
				"type":         "embedding_model",
				"data":         map[string]any{"name": "autotest-embed"},
			},
			field: "data.ai_credentials",
		},
		{
			name:  "an embedding model whose credential link is a bare title",
			title: "autotest_embedding_bad_link",
			body: map[string]any{
				"elitea_title": "autotest_embedding_bad_link",
				"label":        "autotest embedding",
				"type":         "embedding_model",
				"data":         map[string]any{"name": "autotest-embed", "ai_credentials": "autotest_cred"},
			},
			field: "data.ai_credentials",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := createEchoDo(t, router, testCase.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("create status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), testCase.field) {
				t.Errorf("the refusal must name %q: %s", testCase.field, recorder.Body.String())
			}
			assertNoConfigurationRow(t, pool, testCase.title)
		})
	}
}

func TestCreateAcceptsTheSmallestLegalBodyOfEachSection(t *testing.T) {
	pool := newCreateEchoPool(t)
	router := createEchoRouter(pool)

	cases := []struct {
		name    string
		body    map[string]any
		section string
	}{
		{
			name: "an Azure OpenAI credential with only its endpoint",
			body: map[string]any{
				"elitea_title": "autotest_azure_minimal",
				"label":        "autotest azure",
				"type":         "azure_open_ai",
				"data":         map[string]any{"api_base": "https://autotest.invalid"},
			},
			section: "ai_credentials",
		},
		{
			name: "a Bedrock credential carrying only a region",
			body: map[string]any{
				"elitea_title": "autotest_bedrock_region",
				"label":        "autotest bedrock",
				"type":         "amazon_bedrock",
				"data":         map[string]any{"aws_region_name": "us-east-1"},
			},
			section: "ai_credentials",
		},
		{
			name: "a vector store with no connection string",
			body: map[string]any{
				"elitea_title": "autotest_pgvector_minimal",
				"label":        "autotest pgvector",
				"type":         "pgvector",
				"data":         map[string]any{},
			},
			section: "vectorstorage",
		},
		{
			name: "an embedding model naming a model and a credential",
			body: map[string]any{
				"elitea_title": "autotest_embedding_minimal",
				"label":        "autotest embedding",
				"type":         "embedding_model",
				"data": map[string]any{
					"name":           "autotest-embed",
					"ai_credentials": map[string]any{"elitea_title": "autotest_azure_minimal", "private": false},
				},
			},
			section: "embedding",
		},
		{
			name: "a toolkit credential with only its base URL",
			body: map[string]any{
				"elitea_title": "autotest_github_minimal",
				"label":        "autotest github",
				"type":         "github",
				"data":         map[string]any{"base_url": "https://autotest.invalid/api"},
			},
			section: "credentials",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := createEchoDo(t, router, testCase.body)
			if recorder.Code != http.StatusCreated {
				t.Fatalf("create status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			var echo map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &echo); err != nil {
				t.Fatalf("decode create response %q: %v", recorder.Body.String(), err)
			}
			// The section is derived from the TYPE and decides which screen
			// shows the row. It is asserted here because every accepted body
			// above is the SMALLEST legal one, and a body that carries no
			// `section` is the shape the product's own forms send.
			if echo["section"] != testCase.section {
				t.Errorf("section = %#v, want %q", echo["section"], testCase.section)
			}
		})
	}
}

// TestCreateStillStoresTheModelPickersCompatibilityRow is the other fail-open
// edge, and the one a deployment depends on: deploy/scripts/seed-llm-api.py
// writes a `section: "models"` row whose TYPE is a credential type and whose
// data is `{model, ai_credentials}`. That body is not an instance of the
// credential's own schema and must not be judged as one — but the four row
// fields still apply, which the second half asserts.
func TestCreateStillStoresTheModelPickersCompatibilityRow(t *testing.T) {
	pool := newCreateEchoPool(t)
	router := createEchoRouter(pool)

	accepted := createEchoDo(t, router, map[string]any{
		"elitea_title": "autotest_model-picker",
		"label":        "autotest model",
		"type":         "open_ai",
		"section":      "models",
		"data": map[string]any{
			"model":          "autotest-model",
			"ai_credentials": map[string]any{"elitea_title": "autotest_cred", "private": false},
		},
	})
	if accepted.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", accepted.Code, accepted.Body.String())
	}

	refused := createEchoDo(t, router, map[string]any{
		"elitea_title": "autotest_model-picker-no-label",
		"type":         "open_ai",
		"section":      "models",
		"data":         map[string]any{"model": "autotest-model"},
	})
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("create status = %d, body = %s", refused.Code, refused.Body.String())
	}
	if !strings.Contains(refused.Body.String(), "label") {
		t.Errorf("the refusal must name the missing field: %s", refused.Body.String())
	}
}

// TestCreateStillStoresATypeTheRegistryDoesNotCarry pins the fail-open edge.
// A type with no schema has no requirements to be missing, and refusing it
// would turn "this build has not heard of your type" into "your body is
// wrong", which is a different and untrue statement.
func TestCreateStillStoresATypeTheRegistryDoesNotCarry(t *testing.T) {
	pool := newCreateEchoPool(t)
	router := createEchoRouter(pool)

	recorder := createEchoDo(t, router, map[string]any{
		"elitea_title": "autotest_unknown_type",
		"type":         "autotest_not_in_the_registry",
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func assertNoConfigurationRow(t *testing.T, pool *pgxpool.Pool, title string) {
	t.Helper()
	var rows int
	statement := fmt.Sprintf(
		"SELECT count(*) FROM p_%d.configuration WHERE elitea_title = $1", globalScopeProject)
	if err := pool.QueryRow(context.Background(), statement, title).Scan(&rows); err != nil {
		t.Fatalf("count the rows the refusal must not have written: %v", err)
	}
	if rows != 0 {
		t.Errorf("a refused create wrote %d row(s) for %q", rows, title)
	}
}
