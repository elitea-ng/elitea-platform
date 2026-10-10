package run_test

// Deleting a wiki's search index (issue #1243, ADR-0031 phase D0).
//
// delete_wiki used to remove the artifact objects only; the engine's index
// of the wiki (nodes, edges, embeddings, BM25 rows) stayed for ever. It now
// calls the engine's delete_wiki_index after the objects are gone and reports
// both halves. delete_project_wikis is the entry point for project
// deprovisioning: a platform call, signed with the project and no user.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/engine"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// engineIndex stands in for the engine's delete_wiki_index.
type engineIndex struct {
	mu     sync.Mutex
	wikis  []string
	result map[string]any
	err    error
}

func (e *engineIndex) tool(_ context.Context, arguments map[string]any, _ *spi.Context) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.wikis = append(e.wikis, arguments["wiki_id"].(string))
	if e.err != nil {
		return nil, e.err
	}
	return e.result, nil
}

func (e *engineIndex) calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.wikis...)
}

func indexedWiki() *fakeStore {
	return newStore(map[string]string{
		"acme--gone--main/wiki_manifest_1.json": "{}",
		"acme--gone--main/wiki_pages/a.md":      "# a",
	})
}

func removed(rows float64) map[string]any {
	return map[string]any{"success": true, "deleted": true, "rows": map[string]any{"nodes": rows, "edges": 0.0, "wikis": 1.0}}
}

func TestDeleteWikiReportsTheArtifactsAndTheIndex(t *testing.T) {
	engine := &engineIndex{result: removed(40)}
	store := indexedWiki()
	runner := wikiRunner(store, run.WikiQueryDeps{DeleteIndex: engine.tool})
	_, got := answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--gone--main"})
	want := "Wiki 'acme--gone--main' successfully deleted.\n- Objects removed: 2\n- Registry updated: No\n- Search index removed: Yes (41 rows)"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	if calls := engine.calls(); len(calls) != 1 || calls[0] != "acme--gone--main" {
		t.Fatalf("the engine was asked to delete %v", calls)
	}
	// The index is deleted AFTER the artifacts: by then the bucket is empty.
	if len(store.objects) != 0 {
		t.Fatalf("objects survived: %v", store.objects)
	}
}

func TestDeleteWikiSaysWhenThereWasNoIndex(t *testing.T) {
	engine := &engineIndex{result: map[string]any{"success": true, "deleted": false, "rows": map[string]any{}}}
	runner := wikiRunner(indexedWiki(), run.WikiQueryDeps{DeleteIndex: engine.tool})
	_, got := answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--gone--main"})
	if !strings.HasSuffix(got, "- Search index removed: No (the wiki had no search index)") {
		t.Fatalf("%q", got)
	}
}

// A failed index delete leaves the half-deleted state the tool exists to
// avoid, so it is named, with the way out; and the retry, which finds no
// objects, finishes the job.
func TestDeleteWikiNamesAFailedIndexDeleteAndTheRetryFinishesIt(t *testing.T) {
	engine := &engineIndex{err: errors.New("the index database refused it")}
	store := indexedWiki()
	runner := wikiRunner(store, run.WikiQueryDeps{DeleteIndex: engine.tool})
	_, got := answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--gone--main"})
	for _, part := range []string{"deletion completed with errors", "Objects removed: 2", "Search index: removing it failed: the index database refused it", "Retry the delete"} {
		if !strings.Contains(got, part) {
			t.Errorf("missing %q in %q", part, got)
		}
	}
	if len(store.objects) != 0 {
		t.Fatal("the artifacts are gone, as reported")
	}

	engine.mu.Lock()
	engine.err, engine.result = nil, removed(40)
	engine.mu.Unlock()
	_, got = answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--gone--main"})
	if !strings.Contains(got, "has no objects in the bucket") || !strings.Contains(got, "Search index removed: Yes (41 rows)") {
		t.Fatalf("the retry did not remove the index: %q", got)
	}
}

// With objects left over, the wiki is still listed and answerable; the
// index goes with the last of them.
func TestDeleteWikiKeepsTheIndexWhileObjectsRemain(t *testing.T) {
	engine := &engineIndex{result: removed(40)}
	store := indexedWiki()
	store.undeletable["acme--gone--main/wiki_pages/a.md"] = true
	runner := wikiRunner(store, run.WikiQueryDeps{DeleteIndex: engine.tool})
	_, got := answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--gone--main"})
	if !strings.Contains(got, "the keys above remain") || !strings.Contains(got, "The search index was kept") {
		t.Fatalf("%q", got)
	}
	if calls := engine.calls(); len(calls) != 0 {
		t.Fatalf("the index was deleted while objects remain: %v", calls)
	}
}

// A wiki with no objects and no index is "not found", the legacy text.
func TestDeleteWikiWithNeitherObjectsNorIndexIsNotFound(t *testing.T) {
	engine := &engineIndex{result: map[string]any{"success": true, "deleted": false, "rows": map[string]any{}}}
	runner := wikiRunner(newStore(map[string]string{}), run.WikiQueryDeps{DeleteIndex: engine.tool})
	_, got := answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--nope--main"})
	if got != "Wiki 'acme--nope--main' not found in registry." {
		t.Fatalf("%q", got)
	}
	if calls := engine.calls(); len(calls) != 1 {
		t.Fatalf("the index of a wiki with no objects must still be looked for: %v", calls)
	}
}

// A rolling deploy: the host is newer than the engine, which answers
// "Unknown tool" for delete_wiki_index. That is reported, with a warning,
// not failed: the artifacts ARE deleted.
func TestDeleteWikiReportsAnEngineThatCannotDeleteAnIndex(t *testing.T) {
	old := func(context.Context, map[string]any, *spi.Context) (map[string]any, error) {
		return nil, spi.NewFailure(spi.KindRuntime, fmt.Errorf("The DeepWiki engine refused the invocation: HTTP 400 Unknown tool: delete_wiki_index: %w", engine.ErrUnknownTool))
	}
	runner := wikiRunner(indexedWiki(), run.WikiQueryDeps{DeleteIndex: old})
	_, got := answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--gone--main"})
	want := "Wiki 'acme--gone--main' successfully deleted.\n- Objects removed: 2\n- Registry updated: No\n- Search index: search index cleanup unavailable on this engine version"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	// And with no objects at all: not a failure either.
	runner = wikiRunner(newStore(map[string]string{}), run.WikiQueryDeps{DeleteIndex: old})
	_, got = answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--nope--main"})
	if !strings.Contains(got, "not found in registry") || !strings.Contains(got, "search index cleanup unavailable on this engine version") ||
		strings.Contains(got, "failed") {
		t.Fatalf("%q", got)
	}
}

// `deleted` is true when ANY row was removed, even with no wikis row: the
// host reports the engine's counts, not a guessed extra row.
func TestDeleteWikiReportsTheEnginesActualCounts(t *testing.T) {
	stray := &engineIndex{result: map[string]any{"success": true, "deleted": true,
		"rows": map[string]any{"nodes": 0.0, "edges": 0.0, "embeddings": 0.0, "statistics": 3.0, "wikis": 0.0}}}
	runner := wikiRunner(newStore(map[string]string{}), run.WikiQueryDeps{DeleteIndex: stray.tool})
	_, got := answer(t, runner, "delete_wiki", map[string]any{"wiki_id": "acme--stray--main"})
	if !strings.Contains(got, "Search index removed: Yes (3 rows)") {
		t.Fatalf("%q", got)
	}
}

func platformServer(t *testing.T, sidecar *fakeSidecar) *spi.Server {
	t.Helper()
	settings, err := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", func(key string) (string, bool) {
		switch key {
		case "ELITEA_DEEPWIKI_ENGINE_SOCKET":
			return sidecar.socket, true
		case "ELITEA_DEEPWIKI_IDENTITY_SECRET":
			return identitySecret, true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := spi.NewServer(settings, deepwiki.App(run.NewEngineRunner(settings)), nil)
	if err != nil {
		t.Fatal(err)
	}
	server.Start(t.Context())
	t.Cleanup(server.Stop)
	return server
}

func invokeSigned(t *testing.T, server *spi.Server, identity spi.Identity, tool string) map[string]any {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/tools/wiki_query/"+tool+"/invoke",
		bytes.NewReader([]byte(`{"parameters": {}}`)))
	spi.SignHeaders(request.Header, identity, []byte(identitySecret))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	var accepted map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &accepted)
	id, _ := accepted["invocation_id"].(string)
	if id == "" {
		t.Fatalf("not accepted: %d %s", recorder.Code, recorder.Body.String())
	}
	return pollUntilTerminal(t, server, "/tools/wiki_query/"+tool+"/invocations/"+id)
}

// ---------------------------------------------------------------------------
// The toolkit no longer carries the project-wide delete
// ---------------------------------------------------------------------------

func TestTheWikiQueryToolkitDoesNotListProjectDeletion(t *testing.T) {
	for _, list := range [][]string{run.WikiQueryToolNames, run.EngineTools, deepwiki.Toolkits.Families[2].Tools} {
		for _, name := range list {
			if name == "delete_project_wikis" {
				t.Fatalf("delete_project_wikis is still in %v", list)
			}
		}
	}
	// And the runner does not serve it as a tool.
	sidecar := newFakeSidecar(t, []string{`{"result": {"success": true, "wikis": []}}`}, 0)
	server := platformServer(t, sidecar)
	body := invokeSigned(t, server, spi.Identity{ProjectID: "17"}, "delete_project_wikis")
	if body["status"] != "Error" {
		t.Fatalf("a toolkit call ran the project deletion: %v", body)
	}
	sidecar.mu.Lock()
	defer sidecar.mu.Unlock()
	if len(sidecar.requests) != 0 {
		t.Fatalf("the engine was called %d time(s)", len(sidecar.requests))
	}
}

// ---------------------------------------------------------------------------
// The platform route: POST /internal/v1/projects/delete, over mTLS
// ---------------------------------------------------------------------------

type testPKI struct {
	ca       *x509.Certificate
	caKey    *ecdsa.PrivateKey
	caPool   *x509.CertPool
	serial   int64
	serverTC tls.Certificate
}

func newPKI(t *testing.T, name string) *testPKI {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	p := &testPKI{ca: ca, caKey: key, caPool: pool, serial: 1}
	p.serverTC = p.leaf(t, "host", x509.ExtKeyUsageServerAuth)
	return p
}

func (p *testPKI) leaf(t *testing.T, cn string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	p.serial++
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(p.serial), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// platformHost serves the deepwiki host over mutual TLS, as the listener
// does, with the given allowlist.
func platformHost(t *testing.T, sidecar *fakeSidecar, pki *testPKI, clients string) *httptest.Server {
	t.Helper()
	settings, err := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", func(key string) (string, bool) {
		switch key {
		case "ELITEA_DEEPWIKI_ENGINE_SOCKET":
			return sidecar.socket, true
		case "ELITEA_DEEPWIKI_IDENTITY_SECRET":
			return identitySecret, true
		case "ELITEA_DEEPWIKI_PLATFORM_CLIENTS":
			return clients, clients != ""
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	// What the listener's settings say when TLS_CERTFILE/KEYFILE/CA_FILE are set.
	settings.TLSCertFile, settings.TLSKeyFile, settings.TLSCAFile = "cert", "key", "ca"
	server, err := spi.NewServer(settings, deepwiki.App(run.NewEngineRunner(settings)), nil)
	if err != nil {
		t.Fatal(err)
	}
	server.Start(t.Context())
	t.Cleanup(server.Stop)
	host := httptest.NewUnstartedServer(server)
	host.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverTC}, ClientCAs: pki.caPool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	host.Config.ErrorLog = log.New(io.Discard, "", 0)
	host.StartTLS()
	t.Cleanup(host.Close)
	return host
}

func callRoute(host *httptest.Server, pki *testPKI, cert *tls.Certificate, body string, headers http.Header) (int, string, error) {
	cfg := &tls.Config{RootCAs: pki.caPool, MinVersion: tls.VersionTLS12}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 10 * time.Second}
	request, _ := http.NewRequest(http.MethodPost, host.URL+spi.DeleteProjectPath, strings.NewReader(body))
	for k, v := range headers {
		request.Header[k] = v
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = response.Body.Close() }()
	text, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(text), nil
}

func engineCalls(sidecar *fakeSidecar) int {
	sidecar.mu.Lock()
	defer sidecar.mu.Unlock()
	return len(sidecar.requests)
}

func TestProjectDeprovisioningIsAPlatformRouteAuthorisedByTheMainCertificate(t *testing.T) {
	sidecar := newFakeSidecar(t, []string{
		`{"result": {"success": true, "project_id": 17, "wikis": ["acme--a--main", "acme--b--main"], "rows": {"nodes": 5, "wikis": 2}, "builds": 1, "live_builds": 0}}`,
	}, 0)
	pki := newPKI(t, "platform ca")
	host := platformHost(t, sidecar, pki, "elitea-main")
	main := pki.leaf(t, "elitea-main", x509.ExtKeyUsageClientAuth)

	status, text, err := callRoute(host, pki, &main, `{"project_id": 17}`, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("%d %s %v", status, text, err)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(text), &got)
	if got["project_id"] != "17" || got["wikis"] != float64(2) || got["builds"] != float64(1) {
		t.Fatalf("%v", got)
	}
	sidecar.mu.Lock()
	call := sidecar.requests[0]
	sidecar.mu.Unlock()
	arguments, _ := call["arguments"].(map[string]any)
	if call["tool"] != "delete_project_wikis" || arguments[run.ProjectArgument] != "17" {
		t.Fatalf("the engine was called as %v", call)
	}
	// The id as a string is accepted too (JSON clients differ).
	if status, text, _ := callRoute(host, pki, &main, `{"project_id": "17"}`, nil); status != http.StatusOK {
		t.Fatalf("%d %s", status, text)
	}
}

func TestOnlyTheMainCertificateMayDeleteAProject(t *testing.T) {
	sidecar := newFakeSidecar(t, []string{`{"result": {"success": true, "wikis": []}}`}, 0)
	pki := newPKI(t, "platform ca")
	host := platformHost(t, sidecar, pki, "elitea-main")
	body := `{"project_id": 17}`

	// A certificate from the right CA but another service: refused (403).
	facade := pki.leaf(t, "elitea-facade", x509.ExtKeyUsageClientAuth)
	if status, _, err := callRoute(host, pki, &facade, body, nil); err != nil || status != http.StatusForbidden {
		t.Fatalf("another platform service got %d %v", status, err)
	}
	// ... even when it signs a project and NO user: the old inference.
	signed := http.Header{}
	spi.SignHeaders(signed, spi.Identity{ProjectID: "17"}, []byte(identitySecret))
	if status, _, err := callRoute(host, pki, &facade, body, signed); err != nil || status != http.StatusForbidden {
		t.Fatalf("a signed call with no user id got %d %v", status, err)
	}
	// ... or a user session.
	user := http.Header{}
	spi.SignHeaders(user, spi.Identity{ProjectID: "17", UserID: "5"}, []byte(identitySecret))
	if status, _, err := callRoute(host, pki, &facade, body, user); err != nil || status != http.StatusForbidden {
		t.Fatalf("a user session got %d %v", status, err)
	}
	// No client certificate: the handshake fails.
	if _, _, err := callRoute(host, pki, nil, body, nil); err == nil {
		t.Fatal("a call with no client certificate was accepted")
	}
	// The right name from a CA the host does not trust: the handshake fails.
	foreign := newPKI(t, "foreign ca")
	impostor := foreign.leaf(t, "elitea-main", x509.ExtKeyUsageClientAuth)
	if _, _, err := callRoute(host, pki, &impostor, body, nil); err == nil {
		t.Fatal("a certificate from another CA was accepted")
	}
	if n := engineCalls(sidecar); n != 0 {
		t.Fatalf("the engine was called %d time(s) for refused callers", n)
	}
}

func TestThePlatformRouteIsClosedWithoutAnAllowlistAndWithoutTLS(t *testing.T) {
	sidecar := newFakeSidecar(t, []string{`{"result": {"success": true, "wikis": []}}`}, 0)
	pki := newPKI(t, "platform ca")
	// No allowlist configured: even elitea-main's name is refused.
	host := platformHost(t, sidecar, pki, "")
	main := pki.leaf(t, "elitea-main", x509.ExtKeyUsageClientAuth)
	if status, _, err := callRoute(host, pki, &main, `{"project_id": 17}`, nil); err != nil || status != http.StatusForbidden {
		t.Fatalf("with no allowlist: %d %v", status, err)
	}
	// A cleartext in-process request (no TLS state at all) is refused.
	plain := platformServer(t, sidecar)
	request := httptest.NewRequest(http.MethodPost, spi.DeleteProjectPath, strings.NewReader(`{"project_id": 17}`))
	recorder := httptest.NewRecorder()
	plain.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cleartext: %d", recorder.Code)
	}
	if n := engineCalls(sidecar); n != 0 {
		t.Fatalf("the engine was called %d time(s)", n)
	}
}

func TestThePlatformRouteValidatesTheProjectInTheBody(t *testing.T) {
	sidecar := newFakeSidecar(t, []string{`{"result": {"success": true, "wikis": []}}`}, 0)
	pki := newPKI(t, "platform ca")
	host := platformHost(t, sidecar, pki, "elitea-main")
	main := pki.leaf(t, "elitea-main", x509.ExtKeyUsageClientAuth)
	for _, body := range []string{``, `{}`, `{"project_id": 0}`, `{"project_id": -4}`, `{"project_id": "abc"}`, `{"project_id": 1.5}`, `not json`} {
		if status, _, err := callRoute(host, pki, &main, body, nil); err != nil || status != http.StatusBadRequest {
			t.Errorf("%q: %d %v", body, status, err)
		}
	}
	if n := engineCalls(sidecar); n != 0 {
		t.Fatalf("the engine was called %d time(s)", n)
	}
}
