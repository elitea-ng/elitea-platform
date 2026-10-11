package run_test

// The Inventory platform service (issue #1244): graph deletion over gRPC with
// mutual TLS (elitea.subapp.v1.PlatformOperations), authorised by the
// verified client certificate and nothing else. The server, the TLS and the
// invocation manager are real; the engine is a table of tools that records
// what it was asked.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	subappv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/subapp/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/inventory"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/inventory/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

type pki struct {
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	pool   *x509.CertPool
	serial int64
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &pki{ca: ca, caKey: key, pool: pool, serial: 1}
}

func (p *pki) leaf(t *testing.T, cn string, usage x509.ExtKeyUsage) tls.Certificate {
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

// engineLog records the platform tools the fake engine was asked to run.
type engineLog struct {
	mu    sync.Mutex
	calls []map[string]any
	// answer is the engine's reply to a delete tool.
	answer func(tool string, arguments map[string]any) (map[string]any, error)
}

func (e *engineLog) tool(name string) run.Tool {
	return func(_ context.Context, arguments map[string]any, _ *spi.Context) (map[string]any, error) {
		e.mu.Lock()
		e.calls = append(e.calls, arguments)
		e.mu.Unlock()
		if e.answer != nil {
			return e.answer(name, arguments)
		}
		return nil, fmt.Errorf("no answer for %s", name)
	}
}

func (e *engineLog) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

func engineDocument(value map[string]any) map[string]any {
	text, _ := json.Marshal(value)
	return map[string]any{"success": true, "result": string(text)}
}

// platformRig is an Inventory runner over a fake engine and a real manager,
// served on a loopback mTLS gRPC listener for the clients on the allowlist.
type platformRig struct {
	runner  *run.Runner
	manager *spi.Manager
	engine  *engineLog
	pki     *pki
	addr    string
}

func newPlatformRig(t *testing.T, clients []string) *platformRig {
	t.Helper()
	engine := &engineLog{answer: func(tool string, arguments map[string]any) (map[string]any, error) {
		if tool == "delete_graph" {
			return engineDocument(map[string]any{"deleted": true, "project_id": 7, "application_id": 70,
				"entities": 5, "relations": 4, "sources": 2, "documents": 3}), nil
		}
		return engineDocument(map[string]any{"project_id": 7, "toolkits": []int{70, 71},
			"entities": 9, "relations": 8, "sources": 4, "documents": 6}), nil
	}}
	p := newPKI(t)
	runner := &run.Runner{
		RunnerName: "sidecar", RequireVerifiedProject: true,
		DeleteGraph:         engine.tool("delete_graph"),
		DeleteProjectGraphs: engine.tool("delete_project_graphs"),
		Tools: map[string]run.Tool{"run_ingestion": func(ctx context.Context, _ map[string]any, tc *spi.Context) (map[string]any, error) {
			return nil, fmt.Errorf("replaced per test")
		}},
		StopWait: 2 * time.Second,
	}
	manager := spi.NewManager(nil, time.Hour, nil)
	manager.Start(t.Context())
	t.Cleanup(manager.Stop)
	runner.AttachManager(manager)

	serverCert := p.leaf(t, "host", x509.ExtKeyUsageServerAuth)
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientCAs: p.pool,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	server, err := spi.NewPlatformGRPCServer(runner, clients, tlsConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return &platformRig{runner: runner, manager: manager, engine: engine, pki: p, addr: listener.Addr().String()}
}

func (r *platformRig) client(t *testing.T, cn string) subappv1.PlatformOperationsClient {
	t.Helper()
	cfg := &tls.Config{RootCAs: r.pki.pool, MinVersion: tls.VersionTLS12}
	if cn != "" {
		cfg.Certificates = []tls.Certificate{r.pki.leaf(t, cn, x509.ExtKeyUsageClientAuth)}
	}
	conn, err := grpc.NewClient(r.addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return subappv1.NewPlatformOperationsClient(conn)
}

func callCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

func (r *platformRig) deleteToolkit(c subappv1.PlatformOperationsClient, project, toolkit int32) (*subappv1.DeleteToolkitResponse, error) {
	ctx, cancel := callCtx()
	defer cancel()
	return c.DeleteToolkit(ctx, &subappv1.DeleteToolkitRequest{ProjectId: project, ToolkitId: toolkit})
}

func (r *platformRig) deleteProject(c subappv1.PlatformOperationsClient, project int32) (*subappv1.DeleteProjectResponse, error) {
	ctx, cancel := callCtx()
	defer cancel()
	return c.DeleteProject(ctx, &subappv1.DeleteProjectRequest{ProjectId: project})
}

// startIngest runs run_ingestion for (project, toolkit) through the manager
// until it is stopped; obeys decides whether it honours the stop.
func (r *platformRig) startIngest(t *testing.T, project string, toolkit any, obeys bool) *spi.Invocation {
	t.Helper()
	started := make(chan struct{})
	r.runner.Tools["run_ingestion"] = func(ctx context.Context, _ map[string]any, tc *spi.Context) (map[string]any, error) {
		close(started)
		if !obeys {
			// An ingest that ignores the stop, for longer than any wait here.
			time.Sleep(4 * time.Second)
			return map[string]any{"success": true, "result": "late"}, nil
		}
		for {
			if err := tc.Checkpoint(); err != nil {
				return nil, err
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	family, _ := inventory.Toolkits.Resolve("inventory")
	request := map[string]any{"configuration": map[string]any{"application_id": toolkit},
		"parameters": map[string]any{"source": map[string]any{"toolkit": "git"}}}
	invocation, err := r.manager.Submit(t.Context(), "inventory", "run_ingestion", func(ctx context.Context, tc *spi.Context) (map[string]any, error) {
		return r.runner.Invoke(ctx, spi.Invoke{Family: family, Toolkit: "inventory", Tool: "run_ingestion",
			Request: request, Identity: spi.Identity{ProjectID: project}}, tc)
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the ingest never started")
	}
	return invocation
}

func TestGraphDeletionIsGRPCAuthorisedByTheMainCertificate(t *testing.T) {
	rig := newPlatformRig(t, []string{"elitea-main"})
	main := rig.client(t, "elitea-main")

	toolkit, err := rig.deleteToolkit(main, 7, 70)
	if err != nil {
		t.Fatal(err)
	}
	if !toolkit.GetDeleted() || toolkit.GetRemoved().GetEntities() != 5 || toolkit.GetRemoved().GetDocuments() != 3 ||
		toolkit.GetProjectId() != 7 || toolkit.GetToolkitId() != 70 {
		t.Fatalf("%v", toolkit)
	}
	call := rig.engine.calls[0]
	if call["project_id"] != int32(7) || call["application_id"] != int32(70) || call["family"] != "platform" || call["tool"] != "delete_graph" {
		t.Fatalf("the engine was asked %v", call)
	}
	if params, _ := call["params"].(map[string]any); params["output_format"] != "json" {
		t.Fatalf("params = %v", call["params"])
	}

	project, err := rig.deleteProject(main, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(project.GetGraphToolkitIds()) != 2 || project.GetGraphToolkitIds()[1] != 71 ||
		project.GetGraphs().GetEntities() != 9 || project.GetGraphs().GetRelations() != 8 {
		t.Fatalf("%v", project)
	}
	if last := rig.engine.calls[1]; last["tool"] != "delete_project_graphs" || last["project_id"] != int32(7) || last["application_id"] != nil {
		t.Fatalf("the engine was asked %v", last)
	}

	// A toolkit with no graph is an empty success.
	rig.engine.answer = func(string, map[string]any) (map[string]any, error) {
		return engineDocument(map[string]any{"deleted": false, "project_id": 7, "application_id": 99,
			"entities": 0, "relations": 0, "sources": 0, "documents": 0}), nil
	}
	none, err := rig.deleteToolkit(main, 7, 99)
	if err != nil || none.GetDeleted() {
		t.Fatalf("%v %v", none, err)
	}
}

func TestOnlyTheAllowedCertificateMayDeleteGraphs(t *testing.T) {
	rig := newPlatformRig(t, []string{"elitea-main"})

	other := rig.client(t, "somebody-else")
	if _, err := rig.deleteToolkit(other, 7, 70); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a verified certificate off the allowlist: %v", err)
	}
	if _, err := rig.deleteProject(other, 7); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a verified certificate off the allowlist: %v", err)
	}
	if _, err := rig.deleteToolkit(rig.client(t, ""), 7, 70); err == nil {
		t.Fatal("a client with no certificate was served")
	}
	if n := rig.engine.count(); n != 0 {
		t.Fatalf("the engine was called %d time(s) for refused callers", n)
	}
}

func TestThePlatformServiceBuildsOnlyWithClientsAndMutualTLS(t *testing.T) {
	p := newPKI(t)
	cert := p.leaf(t, "host", x509.ExtKeyUsageServerAuth)
	mtls := &tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: p.pool, ClientAuth: tls.RequireAndVerifyClientCert}
	runner := &run.Runner{}
	if _, err := spi.NewPlatformGRPCServer(runner, nil, mtls, nil); err == nil {
		t.Fatal("a service was built with no allowed clients")
	}
	for name, cfg := range map[string]*tls.Config{
		"nil":          nil,
		"no client CA": {Certificates: []tls.Certificate{cert}},
		"not required": {Certificates: []tls.Certificate{cert}, ClientCAs: p.pool, ClientAuth: tls.VerifyClientCertIfGiven},
	} {
		if _, err := spi.NewPlatformGRPCServer(runner, []string{"elitea-main"}, cfg, nil); err == nil {
			t.Errorf("%s: a service was built without mutual TLS", name)
		}
	}
}

func TestIdsInTheRequestAreValidated(t *testing.T) {
	rig := newPlatformRig(t, []string{"elitea-main"})
	main := rig.client(t, "elitea-main")
	for _, ids := range [][2]int32{{0, 70}, {-1, 70}, {7, 0}, {7, -3}} {
		if _, err := rig.deleteToolkit(main, ids[0], ids[1]); status.Code(err) != codes.InvalidArgument {
			t.Errorf("toolkit %v: %v", ids, err)
		}
	}
	for _, project := range []int32{0, -5} {
		if _, err := rig.deleteProject(main, project); status.Code(err) != codes.InvalidArgument {
			t.Errorf("project %d: %v", project, err)
		}
	}
	if n := rig.engine.count(); n != 0 {
		t.Fatalf("the engine was called %d time(s) for invalid requests", n)
	}
}

func TestDeletingAToolkitStopsItsRunningIngestsFirst(t *testing.T) {
	rig := newPlatformRig(t, []string{"elitea-main"})
	mine := rig.startIngest(t, "7", float64(70), true)
	// The same project's other toolkit, and another project's same toolkit id.
	sibling := rig.startIngest(t, "7", float64(71), true)
	foreign := rig.startIngest(t, "8", float64(70), true)

	if _, err := rig.deleteToolkit(rig.client(t, "elitea-main"), 7, 70); err != nil {
		t.Fatal(err)
	}
	body, _ := rig.manager.Poll(t.Context(), "inventory", "run_ingestion", mine.ID)
	if body["status"] != "Error" || !strings.Contains(fmt.Sprint(body["result"]), "cancelled") {
		t.Fatalf("the toolkit's ingest was not cancelled: %v", body)
	}
	for name, other := range map[string]*spi.Invocation{"sibling toolkit": sibling, "another project": foreign} {
		if body, _ := rig.manager.Poll(t.Context(), "inventory", "run_ingestion", other.ID); body["status"] == "Error" || body["status"] == "Completed" {
			t.Fatalf("the %s's ingest was stopped: %v", name, body)
		}
	}
	if rig.manager.InFlight() != 2 || rig.engine.count() != 1 {
		t.Fatalf("in flight %d, engine calls %d", rig.manager.InFlight(), rig.engine.count())
	}
}

func TestDeletingAProjectStopsEveryIngestOfThatProject(t *testing.T) {
	rig := newPlatformRig(t, []string{"elitea-main"})
	a := rig.startIngest(t, "7", float64(70), true)
	b := rig.startIngest(t, "7", float64(71), true)
	foreign := rig.startIngest(t, "8", float64(70), true)

	if _, err := rig.deleteProject(rig.client(t, "elitea-main"), 7); err != nil {
		t.Fatal(err)
	}
	for _, mine := range []*spi.Invocation{a, b} {
		if body, _ := rig.manager.Poll(t.Context(), "inventory", "run_ingestion", mine.ID); body["status"] != "Error" {
			t.Fatalf("the project's ingest was not stopped: %v", body)
		}
	}
	if body, _ := rig.manager.Poll(t.Context(), "inventory", "run_ingestion", foreign.ID); body["status"] == "Error" || body["status"] == "Completed" {
		t.Fatalf("another project's ingest was stopped: %v", body)
	}
}

func TestADeletionIsRefusedWhileAnIngestWillNotStop(t *testing.T) {
	rig := newPlatformRig(t, []string{"elitea-main"})
	rig.runner.StopWait = 150 * time.Millisecond
	rig.startIngest(t, "7", float64(70), false)
	main := rig.client(t, "elitea-main")

	_, err := rig.deleteToolkit(main, 7, 70)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "an ingest is still running; retry") {
		t.Fatalf("toolkit: %v", err)
	}
	_, err = rig.deleteProject(main, 7)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "an ingest is still running; retry") {
		t.Fatalf("project: %v", err)
	}
	if n := rig.engine.count(); n != 0 {
		t.Fatalf("the engine deleted (%d call(s)) while an ingest ran", n)
	}
}

func TestTheEnginesLeaseRefusalIsRetryable(t *testing.T) {
	rig := newPlatformRig(t, []string{"elitea-main"})
	main := rig.client(t, "elitea-main")
	for _, message := range []string{
		"an ingestion of this Inventory toolkit is running; stop it and delete the graph again",
		"deleted the Inventory graphs of 1 toolkit(s) of project 7, but an ingestion is running on toolkit(s) 71; stop it and delete the project's graphs again",
	} {
		rig.engine.answer = func(string, map[string]any) (map[string]any, error) {
			return map[string]any{"success": false, "error": message}, nil
		}
		if _, err := rig.deleteToolkit(main, 7, 70); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("toolkit %q: %v", message, err)
		}
		if _, err := rig.deleteProject(main, 7); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("project %q: %v", message, err)
		}
	}
	// Any other engine failure is internal, not retryable.
	rig.engine.answer = func(string, map[string]any) (map[string]any, error) {
		return map[string]any{"success": false, "error": "the store is down"}, nil
	}
	if _, err := rig.deleteToolkit(main, 7, 70); status.Code(err) != codes.Internal {
		t.Errorf("%v", err)
	}
}

// Nothing a user or an agent can reach deletes a graph: the tools are in no
// family, not in the descriptor, and not in the runner's tool table.
func TestNoToolkitToolDeletesAGraph(t *testing.T) {
	for _, name := range []string{"inventory_admin", "platform"} {
		if _, err := inventory.Toolkits.Resolve(name); err == nil {
			t.Errorf("family %q is admitted", name)
		}
	}
	for _, family := range inventory.Toolkits.Families {
		for _, tool := range family.Tools {
			if tool == run.DeleteGraphTool || tool == run.DeleteProjectGraphsTool {
				t.Errorf("the %s family admits %s", family.Name, tool)
			}
		}
	}
	descriptor := fmt.Sprint(inventory.Descriptor("https://host"))
	for _, tool := range []string{run.DeleteGraphTool, run.DeleteProjectGraphsTool} {
		if strings.Contains(descriptor, tool) {
			t.Errorf("the descriptor advertises %s", tool)
		}
		for _, served := range run.EngineTools() {
			if served == tool {
				t.Errorf("EngineTools serves %s as a toolkit tool", tool)
			}
		}
	}
	runner := run.NewEngineRunner(spi.Settings{Prefix: "ELITEA_INVENTORY_", EngineSocket: "/run/inventory/engine.sock"})
	for _, tool := range []string{run.DeleteGraphTool, run.DeleteProjectGraphsTool} {
		if _, ok := runner.Tools[tool]; ok {
			t.Errorf("the engine runner's tool table holds %s", tool)
		}
	}

	// A call that names the tool anyway is refused at the door, the engine
	// never reached.
	h := adminHarness(t)
	h.identity = spi.Identity{ProjectID: "7"}
	for _, tool := range []string{run.DeleteGraphTool, run.DeleteProjectGraphsTool} {
		if body, err := h.invoke("inventory", "inventory", tool, map[string]any{"parameters": map[string]any{}}); err == nil || h.lastArgs != nil {
			t.Fatalf("%s ran as a toolkit tool: %v %v", tool, body, h.lastArgs)
		}
	}
}

func adminHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, map[string]run.Tool{"search_graph": answer(map[string]any{"success": true, "result": "ok"})})
	h.runner.RequireVerifiedProject = true
	h.runner.DeleteGraph = func(context.Context, map[string]any, *spi.Context) (map[string]any, error) {
		h.lastArgs = map[string]any{"called": true}
		return engineDocument(map[string]any{}), nil
	}
	return h
}
