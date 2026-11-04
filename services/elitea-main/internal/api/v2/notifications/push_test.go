package notifications

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/webpush"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

type pushConfigStub webpush.Config

func (stub pushConfigStub) Config(context.Context) webpush.Config { return webpush.Config(stub) }

// pushStoreStub keeps subscriptions by endpoint, with the same idempotency
// and cap rules as the repository (the repository's own test covers SQL).
type pushStoreStub struct {
	rows    []repos.PushSubscription
	ids     map[string]string
	deletes int
	err     error
}

func (stub *pushStoreStub) SaveSubscription(_ context.Context, subscription repos.PushSubscription) (string, bool, error) {
	if stub.err != nil {
		return "", false, stub.err
	}
	if stub.ids == nil {
		stub.ids = map[string]string{}
	}
	for i, row := range stub.rows {
		if row.Endpoint == subscription.Endpoint {
			stub.rows[i] = subscription
			return stub.ids[row.Endpoint], false, nil
		}
	}
	stub.rows = append(stub.rows, subscription)
	if len(stub.rows) > webpush.MaxSubscriptionsPerUser {
		stub.rows = stub.rows[1:]
	}
	stub.ids[subscription.Endpoint] = "id-" + subscription.Endpoint[len(subscription.Endpoint)-2:]
	return stub.ids[subscription.Endpoint], true, nil
}

func (stub *pushStoreStub) DeleteSubscription(_ context.Context, _ int64, endpoint string) error {
	stub.deletes++
	for i, row := range stub.rows {
		if row.Endpoint == endpoint {
			stub.rows = append(stub.rows[:i], stub.rows[i+1:]...)
		}
	}
	return stub.err
}

func newPushTestRoute(t *testing.T, config webpush.Config, store *pushStoreStub, validate PushEndpointValidator, permissions ...string) *PushRoute {
	t.Helper()
	if len(permissions) == 0 {
		permissions = []string{CurrentNotificationsListPermission, CurrentNotificationUpdatePermission}
	}
	route, err := NewPushRoute(pushConfigStub(config), store, validate,
		apimw.AuthConfig{
			PrincipalValidator: currentNotificationPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
				return user, nil
			}),
			FormSessions: authtest.FormSessions{},
		},
		currentNotificationPermissionResolver{userID: 42, permissions: permissions},
	)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func pushRequest(t *testing.T, route http.Handler, method, path string, body any, signedIn bool) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == nil {
		reader = strings.NewReader("")
	} else {
		raw, _ := json.Marshal(body)
		reader = strings.NewReader(string(raw))
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	if signedIn {
		authtest.SignIn(request, "42")
	}
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)
	return response
}

func browserKeys(t *testing.T) map[string]string {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return map[string]string{
		"p256dh": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		"auth":   base64.RawURLEncoding.EncodeToString(auth),
	}
}

const (
	pushConfigURL        = "/api/v2/notifications/push_config/prompt_lib/7"
	pushSubscriptionsURL = "/api/v2/notifications/push_subscriptions/prompt_lib/7"
)

func TestPushConfigAnswersEnabledAndTheKey(t *testing.T) {
	cases := []struct {
		name   string
		config webpush.Config
	}{
		{"on", webpush.Config{Enabled: true, PublicKey: "BPub"}},
		{"switch off", webpush.Config{Enabled: false, PublicKey: "BPub"}},
		{"no key", webpush.Config{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route := newPushTestRoute(t, tc.config, &pushStoreStub{}, nil)
			response := pushRequest(t, route, http.MethodGet, pushConfigURL, nil, true)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d %s", response.Code, response.Body)
			}
			var body map[string]any
			_ = json.Unmarshal(response.Body.Bytes(), &body)
			if body["enabled"] != tc.config.Enabled || body["public_key"] != tc.config.PublicKey || len(body) != 2 {
				t.Fatalf("body = %v", body)
			}
		})
	}
}

func TestPushRoutesNeedASignedInCallerWithPermission(t *testing.T) {
	store := &pushStoreStub{}
	route := newPushTestRoute(t, webpush.Config{Enabled: true}, store, nil)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		path := pushSubscriptionsURL
		if method == http.MethodGet {
			path = pushConfigURL
		}
		if response := pushRequest(t, route, method, path, nil, false); response.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d", method, response.Code)
		}
	}
	denied := newPushTestRoute(t, webpush.Config{Enabled: true}, store, nil, "other.permission")
	if response := pushRequest(t, denied, http.MethodPost, pushSubscriptionsURL,
		map[string]any{"endpoint": "https://push.example.com/a1", "keys": browserKeys(t)}, true); response.Code != http.StatusForbidden {
		t.Fatalf("POST without permission = %d", response.Code)
	}
	if len(store.rows) != 0 {
		t.Fatal("a refused call stored a subscription")
	}
}

func TestPushSubscribeIsIdempotentOnTheEndpoint(t *testing.T) {
	store := &pushStoreStub{}
	route := newPushTestRoute(t, webpush.Config{Enabled: true, PublicKey: "BPub"}, store, nil)
	body := map[string]any{"endpoint": "https://push.example.com/a1", "keys": browserKeys(t)}
	first := pushRequest(t, route, http.MethodPost, pushSubscriptionsURL, body, true)
	if first.Code != http.StatusCreated {
		t.Fatalf("first POST = %d %s", first.Code, first.Body)
	}
	body["keys"] = browserKeys(t)
	second := pushRequest(t, route, http.MethodPost, pushSubscriptionsURL, body, true)
	if second.Code != http.StatusOK || first.Body.String() != second.Body.String() {
		t.Fatalf("second POST = %d %s (first %s)", second.Code, second.Body, first.Body)
	}
	if len(store.rows) != 1 || store.rows[0].P256dh != body["keys"].(map[string]string)["p256dh"] || store.rows[0].UserID != 42 {
		t.Fatalf("rows = %+v", store.rows)
	}
}

func TestPushSubscribeKeepsTenPerUser(t *testing.T) {
	store := &pushStoreStub{}
	route := newPushTestRoute(t, webpush.Config{Enabled: true}, store, nil)
	for i := range 11 {
		endpoint := "https://push.example.com/" + string(rune('a'+i)) + string(rune('a'+i))
		if response := pushRequest(t, route, http.MethodPost, pushSubscriptionsURL,
			map[string]any{"endpoint": endpoint, "keys": browserKeys(t)}, true); response.Code != http.StatusCreated {
			t.Fatalf("POST %d = %d", i, response.Code)
		}
	}
	if len(store.rows) != webpush.MaxSubscriptionsPerUser || store.rows[0].Endpoint != "https://push.example.com/bb" {
		t.Fatalf("rows = %d, first %s", len(store.rows), store.rows[0].Endpoint)
	}
}

func TestPushSubscribeRefusesBadInput(t *testing.T) {
	keys := browserKeys(t)
	cases := []struct {
		name string
		body any
	}{
		{"http endpoint", map[string]any{"endpoint": "http://push.example.com/a", "keys": keys}},
		{"relative endpoint", map[string]any{"endpoint": "/push", "keys": keys}},
		{"user info", map[string]any{"endpoint": "https://user:pw@push.example.com/a", "keys": keys}},
		{"too long", map[string]any{"endpoint": "https://push.example.com/" + strings.Repeat("a", 2048), "keys": keys}},
		{"no keys", map[string]any{"endpoint": "https://push.example.com/a"}},
		{"bad p256dh", map[string]any{"endpoint": "https://push.example.com/a",
			"keys": map[string]string{"p256dh": "AAAA", "auth": keys["auth"]}}},
		{"bad auth", map[string]any{"endpoint": "https://push.example.com/a",
			"keys": map[string]string{"p256dh": keys["p256dh"], "auth": "AAAA"}}},
		{"not json", "nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &pushStoreStub{}
			route := newPushTestRoute(t, webpush.Config{Enabled: true}, store, nil)
			if response := pushRequest(t, route, http.MethodPost, pushSubscriptionsURL, tc.body, true); response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d %s", response.Code, response.Body)
			}
			if len(store.rows) != 0 {
				t.Fatal("a refused body was stored")
			}
		})
	}
}

func TestPushSubscribeRefusesAGuardedEndpointAndADisabledInstall(t *testing.T) {
	body := map[string]any{"endpoint": "https://10.0.0.5/push", "keys": browserKeys(t)}
	store := &pushStoreStub{}
	refuse := func(context.Context, string) error { return errors.New("private address") }
	route := newPushTestRoute(t, webpush.Config{Enabled: true}, store, refuse)
	if response := pushRequest(t, route, http.MethodPost, pushSubscriptionsURL, body, true); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "push_endpoint_refused") {
		t.Fatalf("guarded endpoint = %d %s", response.Code, response.Body)
	}
	route = newPushTestRoute(t, webpush.Config{Enabled: false, PublicKey: "BPub"}, store, nil)
	body["endpoint"] = "https://push.example.com/a"
	if response := pushRequest(t, route, http.MethodPost, pushSubscriptionsURL, body, true); response.Code != http.StatusConflict {
		t.Fatalf("disabled install = %d %s", response.Code, response.Body)
	}
	if len(store.rows) != 0 {
		t.Fatal("a refused subscription was stored")
	}
}

func TestPushUnsubscribeAnswers204EvenWhenAbsent(t *testing.T) {
	store := &pushStoreStub{}
	route := newPushTestRoute(t, webpush.Config{Enabled: true}, store, nil)
	body := map[string]any{"endpoint": "https://push.example.com/a1", "keys": browserKeys(t)}
	pushRequest(t, route, http.MethodPost, pushSubscriptionsURL, body, true)
	for range 2 {
		response := pushRequest(t, route, http.MethodDelete, pushSubscriptionsURL,
			map[string]string{"endpoint": "https://push.example.com/a1"}, true)
		if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
			t.Fatalf("DELETE = %d %s", response.Code, response.Body)
		}
	}
	if len(store.rows) != 0 || store.deletes != 2 {
		t.Fatalf("rows = %d deletes = %d", len(store.rows), store.deletes)
	}
	if response := pushRequest(t, route, http.MethodDelete, pushSubscriptionsURL, map[string]string{}, true); response.Code != http.StatusBadRequest {
		t.Fatalf("DELETE without an endpoint = %d", response.Code)
	}
}
