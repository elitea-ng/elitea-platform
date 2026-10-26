package configurations_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// fakeModelPriceReader models the production reader's contract: prices only
// for a caller the visibility rule admits, and only for names it can price.
type fakeModelPriceReader struct {
	visible map[int64]bool
	prices  map[string]configurationapp.ModelPrice
	err     error
	calls   int
	names   []string
}

func (fake *fakeModelPriceReader) ModelPrices(
	_ context.Context, projectID, userID int64, names []string,
) (map[string]configurationapp.ModelPrice, error) {
	fake.calls++
	fake.names = append([]string(nil), names...)
	if projectID != 7 {
		return nil, errors.New("unexpected project")
	}
	if fake.err != nil {
		return nil, fake.err
	}
	if !fake.visible[userID] {
		return nil, nil
	}
	out := map[string]configurationapp.ModelPrice{}
	for _, name := range names {
		if price, ok := fake.prices[name]; ok {
			out[name] = price
		}
	}
	return out, nil
}

const (
	adminCaller  = "11"
	memberCaller = "12"
)

func pricedCatalogRoute(t *testing.T, prices handler.ModelPriceReader) *handler.CurrentModelCatalogRoute {
	t.Helper()
	reader := &currentModelCatalogReaderStub{
		get: func(context.Context, configurationapp.CurrentModelCatalogQuery) (configurationapp.CurrentModelCatalogResponse, error) {
			return configurationapp.CurrentModelCatalogResponse{
				Total: 2,
				Items: []configurationapp.CurrentModelCatalogItem{
					{Name: "gpt-priced", ProjectID: 7},
					{Name: "mystery-model", ProjectID: 7},
				},
			}, nil
		},
	}
	permissions := permissionResolverFunc(func(_ context.Context, user auth.User, _, _ string) (auth.PermissionResolution, error) {
		userID, _ := user.OwningUserID()
		return auth.PermissionResolution{UserID: userID, Permissions: []string{handler.CurrentModelCatalogPermission}}, nil
	})
	principal := currentReadPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) { return user, nil })
	route, err := handler.NewCurrentModelCatalogRoute(
		reader,
		1,
		apimw.AuthConfig{PrincipalValidator: principal, FormSessions: authtest.FormSessions{}},
		permissions,
		handler.WithModelPrices(prices),
	)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func getCatalog(t *testing.T, route http.Handler, caller, query string) (int, []map[string]any) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v2/configurations/models/7"+query, nil)
	authtest.SignIn(request, caller)
	request.RemoteAddr = "10.0.0.8:43120"
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", response.Body.String(), err)
	}
	return response.Code, body.Items
}

var priceKeys = []string{
	"input_cost_per_1m_tokens", "output_cost_per_1m_tokens",
	"cached_input_cost_per_1m_tokens", "currency",
}

func assertUnpriced(t *testing.T, item map[string]any) {
	t.Helper()
	for _, key := range priceKeys {
		if _, present := item[key]; present {
			t.Fatalf("item %v carries %s", item, key)
		}
	}
}

func TestCurrentModelCatalogRouteServesGatewayPrices(t *testing.T) {
	prices := &fakeModelPriceReader{
		visible: map[int64]bool{11: true},
		prices: map[string]configurationapp.ModelPrice{
			"gpt-priced": {Input: "2.5", Output: "10", CachedInput: "2.5"},
		},
	}
	route := pricedCatalogRoute(t, prices)

	t.Run("caller who may see amounts", func(t *testing.T) {
		code, items := getCatalog(t, route, adminCaller, "")
		if code != http.StatusOK || len(items) != 2 {
			t.Fatalf("status=%d items=%v", code, items)
		}
		want := map[string]any{
			"input_cost_per_1m_tokens":        2.5,
			"output_cost_per_1m_tokens":       10.0,
			"cached_input_cost_per_1m_tokens": 2.5,
			"currency":                        "USD",
		}
		for key, value := range want {
			if !reflect.DeepEqual(items[0][key], value) {
				t.Fatalf("%s = %#v, want %#v (item %v)", key, items[0][key], value, items[0])
			}
		}
		// The model the catalogue cannot price carries nothing.
		assertUnpriced(t, items[1])
		if !reflect.DeepEqual(prices.names, []string{"gpt-priced", "mystery-model"}) {
			t.Fatalf("names asked = %v, want every item in one read", prices.names)
		}
	})

	t.Run("caller who may not see amounts", func(t *testing.T) {
		code, items := getCatalog(t, route, memberCaller, "")
		if code != http.StatusOK || len(items) != 2 {
			t.Fatalf("status=%d items=%v", code, items)
		}
		assertUnpriced(t, items[0])
		assertUnpriced(t, items[1])
	})

	t.Run("non-llm sections never read or carry prices", func(t *testing.T) {
		before := prices.calls
		for _, section := range []string{"embedding", "image_generation", "asr", "tts", "vectorstorage"} {
			code, items := getCatalog(t, route, adminCaller, "?section="+section)
			if code != http.StatusOK || len(items) != 2 {
				t.Fatalf("%s: status=%d items=%v", section, code, items)
			}
			assertUnpriced(t, items[0])
		}
		if prices.calls != before {
			t.Fatalf("price reader called %d times for non-llm sections", prices.calls-before)
		}
	})
}

func TestCurrentModelCatalogRouteServesCatalogueWhenPriceReadFails(t *testing.T) {
	route := pricedCatalogRoute(t, &fakeModelPriceReader{err: errors.New("database is down")})
	code, items := getCatalog(t, route, adminCaller, "")
	if code != http.StatusOK || len(items) != 2 {
		t.Fatalf("status=%d items=%v", code, items)
	}
	assertUnpriced(t, items[0])
	assertUnpriced(t, items[1])
}

func TestCurrentModelCatalogRouteWithoutPriceReaderKeepsShape(t *testing.T) {
	route := pricedCatalogRoute(t, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v2/configurations/models/7", nil)
	authtest.SignIn(request, adminCaller)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "cost") ||
		strings.Contains(response.Body.String(), "currency") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
