package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestDefaultSecretPolicyPreservesVaultOnRefusedWrites(t *testing.T) {
	pool := newSecretsPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `CREATE TABLE centry.platform_config(section text, key text, value jsonb, PRIMARY KEY(section,key));
 INSERT INTO centry.platform_config VALUES ('default_secrets','default_secret_keys','["protected","reserved"]'),('default_secrets','ignore_default_secret_api','true');`)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(pool, WithPlatformDefaultSecretPolicy(pool))
	original := vaultData{Secrets: map[string]string{"protected": "keep", "ordinary": "rotate", SecretsHeaderValueName: "test-header"}, HiddenSecrets: map[string]string{}}
	if err := handler.writeVaultCtx(ctx, "7", original); err != nil {
		t.Fatal(err)
	}
	invoke := func(method string, fn http.HandlerFunc, name, body, header string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "/", strings.NewReader(body))
		route := chi.NewRouteContext()
		route.URLParams.Add("projectID", "7")
		route.URLParams.Add("name", name)
		request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
		request.Header.Set("X-SECRET", header)
		response := httptest.NewRecorder()
		fn(response, request)
		return response
	}
	response := invoke(http.MethodGet, handler.List, "", "", "")
	var items []SecretListItem
	if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Name == "protected" {
			t.Fatal("default name leaked")
		}
	}
	for _, test := range []struct {
		method     string
		fn         http.HandlerFunc
		name, body string
	}{
		{http.MethodPost, handler.Create, "", `{"name":"reserved","value":"new"}`},
		{http.MethodPut, handler.Update, "protected", `{"value":"replace"}`},
		{http.MethodPut, handler.Update, "ordinary", `{"name":"reserved","value":"replace"}`},
		{http.MethodDelete, handler.Delete, "protected", ""},
	} {
		response = invoke(test.method, test.fn, test.name, test.body, "wrong")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", response.Code, response.Body)
		}
		actual, err := handler.readVaultCtx(ctx, "7")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual, original) {
			t.Fatal("refused write changed the vault")
		}
	}
	response = invoke(http.MethodGet, handler.List, "", "", "test-header")
	if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		if item.Name == "protected" {
			found = item.IsDefault
		}
	}
	if !found {
		t.Fatal("default metadata missing")
	}
	response = invoke(http.MethodPut, handler.Update, "ordinary", `{"value":"rotated"}`, "")
	if response.Code != http.StatusOK {
		t.Fatalf("ordinary rotation: %d %s", response.Code, response.Body)
	}
	response = invoke(http.MethodPut, handler.Update, "protected", `{"value":"authorized"}`, "test-header")
	if response.Code != http.StatusOK {
		t.Fatalf("header update: %d %s", response.Code, response.Body)
	}
	response = invoke(http.MethodGet, handler.Get, "ordinary", "", "")
	var detail SecretDetail
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Value != "" {
		t.Fatal("suppressed secret value leaked")
	}
	// Matching the header does not replace the route permission resolver.
	request := httptest.NewRequest(http.MethodGet, "/secrets/default/7", nil)
	request.Header.Set("X-SECRET", "test-header")
	response = httptest.NewRecorder()
	handler.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("missing authorization status=%d", response.Code)
	}
}
