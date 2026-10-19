package repos

// Credentials by reference (D3) on real PostgreSQL: live RBAC of the original
// actor, live configuration revision, vault resolution and the OAuth2
// client-credentials fetch through the HTTP action egress guard.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/egress"
)

type mapUnsecreter map[string]string

func (m mapUnsecreter) Unsecret(_ context.Context, _ int32, data map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for key, value := range data {
		if text, ok := value.(string); ok {
			for name, secret := range m {
				text = strings.ReplaceAll(text, "{{secret."+name+"}}", secret)
			}
			value = text
		}
		out[key] = value
	}
	return out, nil
}

func (w *httpWorld) setConfiguration(data string) {
	w.t.Helper()
	w.exec(`UPDATE p_1.configuration SET data=$1::jsonb, updated_at=clock_timestamp() AT TIME ZONE 'UTC' WHERE id=$2`, data, httpWorldConfig)
}

func TestHTTPCredentialsAPIKeyModesPostgres(t *testing.T) {
	w := newHTTPWorld(t, "GET")
	credentials, err := NewHTTPActionCredentials(w.pool, mapUnsecreter{"k": "key-value"}, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	admitted := w.admission(httpWorldClaim, 1)
	for name, tc := range map[string]struct {
		data   string
		header string
		value  string
		denied bool
	}{
		"bearer":                {`{"api_key":"{{secret.k}}","auth_type":"Bearer"}`, "Authorization", "Bearer key-value", false},
		"default bearer":        {`{"api_key":"{{secret.k}}"}`, "Authorization", "Bearer key-value", false},
		"basic as given":        {`{"api_key":"{{secret.k}}","auth_type":"Basic"}`, "Authorization", "Basic key-value", false},
		"custom header":         {`{"api_key":"{{secret.k}}","auth_type":"custom","custom_header_name":"X-Api-Token"}`, "X-Api-Token", "key-value", false},
		"custom x-api-key":      {`{"api_key":"{{secret.k}}","auth_type":"Custom","custom_header_name":"x-api-key"}`, "X-Api-Key", "key-value", false},
		"anonymous":             {`{}`, "", "", false},
		"custom restricted":     {`{"api_key":"{{secret.k}}","auth_type":"custom","custom_header_name":"cookie"}`, "", "", true},
		"custom host":           {`{"api_key":"{{secret.k}}","auth_type":"custom","custom_header_name":"Host"}`, "", "", true},
		"unresolved secret":     {`{"api_key":"{{secret.missing}}"}`, "", "", true},
		"delegated oauth later": {`{"client_id":"c","client_secret":"s","oauth_discovery_endpoint":"https://idp.example"}`, "", "", true},
		"crlf in key":           {`{"api_key":"a\r\nb"}`, "", "", true},
	} {
		t.Run(name, func(t *testing.T) {
			w.setConfiguration(tc.data)
			credential, err := credentials.Resolve(t.Context(), admitted, httpWorldConfig)
			if tc.denied {
				if app.FailureCode(err) != app.CodePolicyDenied {
					t.Fatalf("not denied: %v", err)
				}
				return
			}
			if err != nil || !app.ValidDigest(credential.Revision) {
				t.Fatal(err)
			}
			if tc.header == "" && len(credential.Headers) != 0 || tc.header != "" && credential.Headers.Get(tc.header) != tc.value {
				t.Fatalf("headers %v", credential.Headers)
			}
		})
	}
}

func TestHTTPCredentialsLiveAuthorityPostgres(t *testing.T) {
	for name, change := range map[string]func(*httpWorld){
		"actor lost the permission": func(w *httpWorld) { w.exec(`DELETE FROM public.auth_core__project_user_role WHERE user_id=11`) },
		"actor suspended":           func(w *httpWorld) { w.exec(`UPDATE public.auth_core__user SET suspended=TRUE WHERE id=11`) },
		"private to another user":   func(w *httpWorld) { w.exec(`UPDATE p_1.configuration SET author_id=12 WHERE id=$1`, httpWorldConfig) },
		"not an openapi type":       func(w *httpWorld) { w.exec(`UPDATE p_1.configuration SET type='github' WHERE id=$1`, httpWorldConfig) },
		"missing configuration":     func(w *httpWorld) { w.exec(`DELETE FROM p_1.configuration WHERE id=$1`, httpWorldConfig) },
	} {
		t.Run(name, func(t *testing.T) {
			w := newHTTPWorld(t, "GET")
			credentials, _ := NewHTTPActionCredentials(w.pool, mapUnsecreter{"k": "v"}, http.DefaultTransport)
			change(w)
			if _, err := credentials.Resolve(t.Context(), w.admission(httpWorldClaim, 1), httpWorldConfig); app.FailureCode(err) != app.CodePolicyDenied {
				t.Fatalf("resolved: %v", err)
			}
		})
	}
	// Shared configurations of other authors are usable; the revision moves
	// with every write so Begin refuses a credential changed after resolve.
	w := newHTTPWorld(t, "GET")
	credentials, _ := NewHTTPActionCredentials(w.pool, mapUnsecreter{"k": "v"}, http.DefaultTransport)
	w.exec(`UPDATE p_1.configuration SET author_id=12, shared=TRUE WHERE id=$1`, httpWorldConfig)
	first, err := credentials.Resolve(t.Context(), w.admission(httpWorldClaim, 1), httpWorldConfig)
	if err != nil {
		t.Fatal(err)
	}
	w.setConfiguration(`{"api_key":"{{secret.k}}","auth_type":"Basic"}`)
	second, err := credentials.Resolve(t.Context(), w.admission(httpWorldClaim, 1), httpWorldConfig)
	if err != nil || second.Revision == first.Revision {
		t.Fatalf("revision did not move: %v", err)
	}
}

// OAuth2 client credentials: fetched by Main through the guard (HTTPS, no
// redirect), Basic or body client authentication, cached per configuration
// revision, refetched after an edit.
func TestHTTPCredentialsOAuthClientCredentialsPostgres(t *testing.T) {
	var fetches atomic.Int32
	token := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		_ = r.ParseForm()
		user, pass, basic := r.BasicAuth()
		// RFC 6749 §2.3.1: Basic parts are form-urlencoded.
		user, _ = url.QueryUnescape(user)
		pass, _ = url.QueryUnescape(pass)
		ok := r.PostForm.Get("grant_type") == "client_credentials" && (basic && user == "client" && pass == "s3cr%t" || r.PostForm.Get("client_id") == "client" && r.PostForm.Get("client_secret") == "s3cr%t")
		if !ok {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": "tok-" + r.PostForm.Get("scope"), "token_type": "Bearer", "expires_in": 3600})
	}))
	defer token.Close()
	redirecting := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, token.URL, http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	guard, err := egress.NewHTTPActionGuard("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	transport := guard.Transport()
	transport.TLSClientConfig = token.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	w := newHTTPWorld(t, "GET")
	credentials, _ := NewHTTPActionCredentials(w.pool, mapUnsecreter{"cs": "s3cr%t"}, transport)
	admitted := w.admission(httpWorldClaim, 1)
	resolve := func() (app.Credential, error) { return credentials.Resolve(t.Context(), admitted, httpWorldConfig) }
	w.setConfiguration(`{"client_id":"client","client_secret":"{{secret.cs}}","token_url":"` + token.URL + `/token","scope":"read","method":"Basic"}`)
	for range 3 {
		credential, err := resolve()
		if err != nil || credential.Headers.Get("Authorization") != "Bearer tok-read" {
			t.Fatalf("%v %v", credential.Headers, err)
		}
	}
	if fetches.Load() != 1 {
		t.Fatalf("token fetched %d times for one revision", fetches.Load())
	}
	w.setConfiguration(`{"client_id":"client","client_secret":"{{secret.cs}}","token_url":"` + token.URL + `/token","scope":"write","method":"default"}`)
	if credential, err := resolve(); err != nil || credential.Headers.Get("Authorization") != "Bearer tok-write" || fetches.Load() != 2 {
		t.Fatalf("edited configuration reused a token: %v %v fetches=%d", credential.Headers, err, fetches.Load())
	}
	w.setConfiguration(`{"client_id":"client","client_secret":"{{secret.cs}}","token_url":"` + redirecting.URL + `/token"}`)
	if _, err := resolve(); !errors.Is(err, app.ErrUnavailable) || fetches.Load() != 2 {
		t.Fatalf("token redirect followed: %v fetches=%d", err, fetches.Load())
	}
	w.setConfiguration(`{"client_id":"client","client_secret":"{{secret.cs}}","token_url":"http://127.0.0.1/token"}`)
	if _, err := resolve(); app.FailureCode(err) != app.CodePolicyDenied {
		t.Fatalf("http token endpoint accepted: %v", err)
	}
	w.setConfiguration(`{"client_id":"client","client_secret":"{{secret.cs}}","token_url":"https://10.0.0.9/token"}`)
	if _, err := resolve(); !errors.Is(err, app.ErrUnavailable) {
		t.Fatalf("private token endpoint dialled: %v", err)
	}
}
