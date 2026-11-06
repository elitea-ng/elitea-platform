package runtimecomposition

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/browsersessions"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/browsersession"
	code "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/security/securefile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/control"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BrowserViewConfigEnv configures the cloud browser view relay (ADR-0049
// decision 6, #1597): Main's client identity toward the browser profiles'
// supervisors, and the supervisors it relays to. Unset keeps the relay off:
// no session is recorded and the view route is not mounted.
const BrowserViewConfigEnv = "ELITEA_RUNTIME_BROWSER_VIEW_CONFIG"

// BrowserViewConfig is the code owner config's shape: no credential and no
// worker-provided address. Every audience must be a sandbox audience.
type BrowserViewConfig struct {
	MainWorkloadIdentity string                `json:"main_workload_identity"`
	CertificateChainPath string                `json:"certificate_chain_path"`
	PrivateKeyPath       string                `json:"private_key_path"`
	ServerCAPath         string                `json:"server_ca_path"`
	Supervisors          []CodeOwnerSupervisor `json:"supervisors"`
	// MaxFPS is the stream's frame rate, at most 10; 0 is 10.
	MaxFPS uint32 `json:"max_fps,omitempty"`
}

func browserViewConfigFromEnv(lookup LookupEnv, enabled, agents bool, audiences []string) (*BrowserViewConfig, error) {
	raw, _ := lookup(BrowserViewConfigEnv)
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 16384 {
		return nil, errors.New("browser view configuration exceeds its bound")
	}
	var cfg BrowserViewConfig
	if code.Decode([]byte(raw), &cfg, 16384) != nil || !browserViewConfigFields([]byte(raw)) {
		return nil, errors.New("browser view configuration is invalid")
	}
	if err := validateBrowserViewConfig(&cfg, enabled, agents, audiences); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func browserViewConfigFields(raw []byte) bool {
	if !code.RequiredFields(raw, []string{"main_workload_identity", "certificate_chain_path", "private_key_path", "server_ca_path", "supervisors"}) {
		return false
	}
	var fields map[string]json.RawMessage
	var supervisors []json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(fields["supervisors"], &supervisors) != nil {
		return false
	}
	for _, supervisor := range supervisors {
		if !code.RequiredFields(supervisor, []string{"audience", "https_origin"}) {
			return false
		}
	}
	return true
}

func validateBrowserViewConfig(cfg *BrowserViewConfig, enabled, agents bool, audiences []string) error {
	if cfg == nil {
		return nil
	}
	if cfg.MaxFPS > 10 {
		return errors.New("browser view max_fps is at most 10")
	}
	// The same rules as the code owner client: active agent dispatch, exact
	// identities, fixed HTTPS origins of configured sandbox audiences.
	owner := CodeOwnerConfig{
		MainWorkloadIdentity: cfg.MainWorkloadIdentity, CertificateChainPath: cfg.CertificateChainPath,
		PrivateKeyPath: cfg.PrivateKeyPath, ServerCAPath: cfg.ServerCAPath, Supervisors: cfg.Supervisors,
	}
	if err := validateCodeOwnerConfig(&owner, enabled, agents, audiences); err != nil {
		return errors.New("browser view requires active agent dispatch, exact identities and supervisors that are sandbox audiences on fixed HTTPS origins")
	}
	return nil
}

// browserViewComposition is the relay's two halves: the control server's
// recorder (the worker's reports) and the public route's dialer.
type browserViewComposition struct {
	sessions  *repos.BrowserSessions
	client    *storage.BrowserViewClient
	audiences []string
}

func configureBrowserView(config Config, dependencies Dependencies, key ed25519.PrivateKey) (*browserViewComposition, error) {
	cfg := config.BrowserView
	if cfg == nil {
		return nil, nil
	}
	if dependencies.ControlPool == nil || dependencies.ReplayPool == nil {
		return nil, errors.New("browser view requires the runtime database pools")
	}
	certificateBytes, err := securefile.Read(cfg.CertificateChainPath, 1<<20, securefile.PublicMaterial)
	if err != nil {
		return nil, errors.New("browser view client TLS identity unavailable")
	}
	keyBytes, err := securefile.Read(cfg.PrivateKeyPath, 1<<20, securefile.PrivateMaterial)
	if err != nil {
		return nil, errors.New("browser view client TLS identity unavailable")
	}
	defer clear(keyBytes)
	certificate, err := tls.X509KeyPair(certificateBytes, keyBytes)
	if err != nil {
		return nil, errors.New("browser view client TLS identity unavailable")
	}
	ca, err := securefile.Read(cfg.ServerCAPath, 1<<20, securefile.PublicMaterial)
	if err != nil {
		return nil, errors.New("browser view server CA unavailable")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("browser view server CA invalid")
	}
	endpoints := make([]storage.BrowserViewEndpoint, 0, len(cfg.Supervisors))
	audiences := make([]string, 0, len(cfg.Supervisors))
	for _, supervisor := range cfg.Supervisors {
		endpoints = append(endpoints, storage.BrowserViewEndpoint{
			Audience: supervisor.Audience, Origin: supervisor.HTTPSOrigin,
			TLS: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate}},
		})
		audiences = append(audiences, supervisor.Audience)
	}
	client, err := storage.NewBrowserViewClient(storage.BrowserViewClientConfig{
		KeyID: config.SigningKeyID, Key: key, Requester: cfg.MainWorkloadIdentity, MaxFPS: cfg.MaxFPS, Endpoints: endpoints,
	})
	if err != nil {
		return nil, errors.New("browser view client configuration rejected")
	}
	sessions, err := repos.NewBrowserSessions(dependencies.ControlPool)
	if err != nil {
		client.Close()
		return nil, err
	}
	return &browserViewComposition{sessions: sessions, client: client, audiences: audiences}, nil
}

// browserSessionRecorder adapts the repository to the control server.
type browserSessionRecorder struct{ sessions *repos.BrowserSessions }

func (r browserSessionRecorder) RecordBrowserSession(ctx context.Context, fence runtimedomain.Fence, report control.BrowserSessionReport) error {
	err := r.sessions.Record(ctx, repos.BrowserSessionReport{
		ExecutionID: fence.ExecutionID, Generation: fence.Generation, SupervisorAudience: report.SupervisorAudience,
		Spec: report.Spec, State: report.State, LingerUntil: report.LingerUntil,
	})
	return mapBrowserSessionError(err)
}

func mapBrowserSessionError(err error) error {
	if errors.Is(err, repos.ErrBrowserSessionRefused) {
		return control.ErrBrowserSessionReportRefused
	}
	return err
}

// browserViewDialer adapts the client to the relay.
type browserViewDialer struct{ client *storage.BrowserViewClient }

func (d browserViewDialer) OpenView(ctx context.Context, session browsersession.Session, control bool) (browsersessions.ViewStream, error) {
	return d.client.OpenView(ctx, session, control)
}

// newBrowserViewHandler reads the session row through the replay pool (any
// replica, any row) and the conversation rule through the observer
// authority.
func newBrowserViewHandler(composition *browserViewComposition, replay *pgxpool.Pool, conversations *repos.ExecutionObserverAuthority, permissions auth.PermissionResolver) (*browsersessions.Handler, error) {
	if composition == nil {
		return nil, nil
	}
	sessions, err := repos.NewBrowserSessions(replay)
	if err != nil {
		return nil, err
	}
	return browsersessions.NewHandler(sessions, conversations, permissions, browserViewDialer{client: composition.client}, slog.Default())
}

// browserViewCloser keeps a nil composition a nil interface (no typed nil).
func browserViewCloser(composition *browserViewComposition) interface{ Close() } {
	if composition == nil {
		return nil
	}
	return composition.client
}
