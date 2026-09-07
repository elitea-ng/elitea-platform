package oapiserver_test

// Regression tests for the REVERSE check's path arm (issue 621).
//
// THE DEFECT. MissingFromSpec resolves a manifest entry that carries no
// operationId by comparing its method+path with the document. It compared
// against SpecOperation.CandidatePaths(), which is `servers[0].url` + the
// document path — "/api/v2/secrets/secrets/{mode}/{projectID}". No entry of
// apps/elitea-web/src/shared/api/endpoints.manifest.json carries that base, so
// the first segment compared "api" with the plugin name and EVERY comparison
// failed.
//
// WHY NOTHING WENT RED. A hand-written entry that fails to match is what
// testdata/reverse_check_allowlist.txt holds, and that file's own rule — "the
// list may only shrink; the test fails on a line the spec now covers" — is the
// only thing this defect could break. It broke it silently: fifteen ids the
// document already described sat on the list, six of them the whole secrets
// domain of issue 151, and the gate reported OK.
//
// The two tests below pin both halves of the repair: the base must not defeat
// a match, and a spec placeholder must not swallow a manifest literal.

import (
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/oapiserver"
)

// TestMissingFromSpecIgnoresTheServerBase is the RED case of the defect: this
// entry is described, and the pre-fix comparison called it missing.
func TestMissingFromSpecIgnoresTheServerBase(t *testing.T) {
	t.Parallel()

	ops := []oapiserver.SpecOperation{{
		OperationID: "listSecrets",
		Method:      "GET",
		Path:        "/secrets/secrets/{mode}/{projectID}",
		BasePaths:   []string{"/api/v2"},
	}}
	endpoints := []oapiserver.ManifestEndpoint{{
		ID:     "secrets.list",
		Method: "GET",
		Path:   "/secrets/secrets/{mode}/{projectID}",
	}}

	if missing := oapiserver.MissingFromSpec(ops, endpoints); len(missing) != 0 {
		t.Fatalf("a described endpoint was reported missing: %+v — the server base is defeating the comparison again", missing)
	}
}

// TestMissingFromSpecKeepsPlaceholderAndLiteralApart is the other direction.
// Both sides are TEMPLATES, so the spec's {bucket} must not stand for the
// manifest's literal pylon mode segment `default`. Reading this as covered
// would have deleted an allowlist line for an endpoint the document does not
// describe.
func TestMissingFromSpecKeepsPlaceholderAndLiteralApart(t *testing.T) {
	t.Parallel()

	ops := []oapiserver.SpecOperation{{
		OperationID: "getBucket",
		Method:      "GET",
		Path:        "/artifacts/buckets/{projectID}/{bucket}",
		BasePaths:   []string{"/api/v2"},
	}}
	endpoints := []oapiserver.ManifestEndpoint{{
		ID:     "artifacts.listBucketMetadata",
		Method: "GET",
		Path:   "/artifacts/buckets/default/{project_id}",
	}}

	missing := oapiserver.MissingFromSpec(ops, endpoints)
	if len(missing) != 1 || missing[0].ID != "artifacts.listBucketMetadata" {
		t.Fatalf("an undescribed endpoint was reported as covered: %+v — a spec placeholder is swallowing a manifest literal again", missing)
	}
}

// TestMissingFromSpecStillDiscriminatesMethodAndShape keeps the repair from
// becoming a rubber stamp: the same path under another method, and a path with
// a different segment count, must both stay missing.
func TestMissingFromSpecStillDiscriminatesMethodAndShape(t *testing.T) {
	t.Parallel()

	ops := []oapiserver.SpecOperation{{
		OperationID: "listSecrets",
		Method:      "GET",
		Path:        "/secrets/secrets/{mode}/{projectID}",
		BasePaths:   []string{"/api/v2"},
	}}
	endpoints := []oapiserver.ManifestEndpoint{
		{ID: "secrets.create", Method: "POST", Path: "/secrets/secrets/{mode}/{projectID}"},
		{ID: "secrets.show", Method: "GET", Path: "/secrets/secrets/{mode}/{projectID}/{name}"},
	}

	if missing := oapiserver.MissingFromSpec(ops, endpoints); len(missing) != 2 {
		t.Fatalf("want both entries missing, got %+v", missing)
	}
}
