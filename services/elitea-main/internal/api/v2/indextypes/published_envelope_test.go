package indextypes_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"testing"

	generated "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/generated"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indextypes"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// TestCurrentIndexTypesRouteAnswersPublishedEnvelopeBesideLegacyKeys is the
// regression test for issue #394.
//
// The route answered {document_types, image_types, code_types} only. The
// published contract for the same path is DocumentLoadersResponse —
// {items, total} — so the shipped generated client read a body with no `items`
// key. That is why the flag stayed dark in every deployment.
//
// The body now carries BOTH halves, the way #395 did it for application
// skills. The two halves project the SAME snapshot rows, so they cannot
// disagree.
func TestCurrentIndexTypesRouteAnswersPublishedEnvelopeBesideLegacyKeys(
	t *testing.T,
) {
	fixture := currentIndexTypesUIFixture(t)
	reader := &currentIndexTypesReaderStub{result: fixture}
	route := newCurrentIndexTypesRoute(
		t,
		reader,
		currentIndexTypesPrincipalValidatorFunc(
			func(_ context.Context, principal auth.User) (auth.User, error) {
				return principal, nil
			},
		),
		currentIndexTypesPermissionResolverFunc(
			func(
				context.Context,
				auth.User,
				string,
				string,
			) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{
					UserID:      11,
					Permissions: []string{handler.CurrentIndexTypesPermission},
				}, nil
			},
		),
	)

	response := httptest.NewRecorder()
	route.ServeHTTP(
		response,
		currentIndexTypesRequest(
			http.MethodGet,
			"/api/v2/elitea_core/index_types/prompt_lib/7",
			true,
			"11",
		),
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}

	// 1. The published half. Decoding into the type oapi-codegen generates
	// from api/openapi/v2.yaml is the same schema the orval/zod client in
	// apps/elitea-web holds, so an empty `items` here is the drift the flag
	// was dark for.
	var published generated.DocumentLoadersResponse
	if err := json.Unmarshal(response.Body.Bytes(), &published); err != nil {
		t.Fatalf("published half does not decode: %v", err)
	}
	if len(published.Items) != 3 || published.Total != len(published.Items) {
		t.Fatalf(
			"published half items=%d total=%d body=%q",
			len(published.Items),
			published.Total,
			response.Body.String(),
		)
	}

	// 2. The legacy half, unchanged: apps/elitea-ui reads exactly these three
	// maps (src/slices/fileTypes.js:26-28).
	var legacyMaps handler.CurrentIndexTypes
	if err := json.Unmarshal(response.Body.Bytes(), &legacyMaps); err != nil {
		t.Fatalf("legacy half does not decode: %v", err)
	}
	if !reflect.DeepEqual(legacyMaps, fixture) {
		t.Fatalf("legacy half drifted from testdata fixture: %+v", legacyMaps)
	}

	// 3. The two halves project the same rows, so they cannot disagree.
	byCategory := map[string]map[string]string{
		"document_types": fixture.DocumentTypes,
		"image_types":    fixture.ImageTypes,
		"code_types":     fixture.CodeTypes,
	}
	for _, item := range published.Items {
		source, known := byCategory[item.Type]
		if !known {
			t.Fatalf("published item names an unknown category %q", item.Type)
		}
		if item.Name == "" || item.Description == "" {
			t.Fatalf("published item %q omits a required field", item.Type)
		}
		want := make([]string, 0, len(source))
		for extension := range source {
			want = append(want, extension)
		}
		slices.Sort(want)
		if !slices.Equal(item.SupportedExtensions, want) {
			t.Fatalf(
				"published item %q lists %v, and the map holds %v",
				item.Type,
				item.SupportedExtensions,
				want,
			)
		}
		delete(byCategory, item.Type)
	}
	if len(byCategory) != 0 {
		t.Fatalf("published half omits categories %v", byCategory)
	}

	// 4. `items` is the key apps/elitea-web unwraps first
	// (shared/api/unwrap.ts). A body without it reports as an unrecognised
	// shape and renders as "no data".
	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &topLevel); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"items", "total", "document_types", "image_types", "code_types",
	} {
		if topLevel[key] == nil {
			t.Fatalf("response omits %q: keys=%v", key, topLevelKeys(topLevel))
		}
	}
	if len(topLevel) != 5 || topLevel["index_types"] != nil {
		t.Fatalf("unexpected top-level keys=%v", topLevelKeys(topLevel))
	}
	if _, err := os.Stat("testdata/current_index_types_ui_response.json"); err != nil {
		t.Fatalf("the pinned SDK fixture must stay: %v", err)
	}
}

func topLevelKeys(body map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
