package auditexport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/observability"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/egress"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Environment variables. Secrets are never values here: the webhook keys and
// the OTLP headers are files, mounted from a Secret.
const (
	EnvOTLPEndpoint      = "ELITEA_AUDIT_EXPORT_OTLP_ENDPOINT"
	EnvOTLPProtocol      = "ELITEA_AUDIT_EXPORT_OTLP_PROTOCOL"
	EnvOTLPHeadersFile   = "ELITEA_AUDIT_EXPORT_OTLP_HEADERS_FILE"
	EnvWebhookURL        = "ELITEA_AUDIT_EXPORT_WEBHOOK_URL"
	EnvWebhookKeysFile   = "ELITEA_AUDIT_EXPORT_WEBHOOK_KEYS_FILE"
	EnvEgressAllowlist   = "ELITEA_AUDIT_EXPORT_EGRESS_ALLOWLIST"
	EnvAuditEvents       = "ELITEA_AUDIT_EXPORT_AUDIT_EVENTS"
	EnvAuditEventsFrom   = "ELITEA_AUDIT_EXPORT_AUDIT_EVENTS_FROM"
	EnvBatchSize         = "ELITEA_AUDIT_EXPORT_BATCH_SIZE"
	EnvInterval          = "ELITEA_AUDIT_EXPORT_INTERVAL"
	EnvMaxPermanent      = "ELITEA_AUDIT_EXPORT_MAX_PERMANENT_ATTEMPTS"
	EnvMetricsAddr       = "ELITEA_AUDIT_EXPORT_METRICS_ADDR"
	maxHeadersFileSize   = 16 << 10
	defaultOTLPProtocol  = "http"
	defaultAuditFromMode = "now"
)

// ErrInvalidConfig refuses boot. Its text names the variable, never a value
// read from a secret file.
var ErrInvalidConfig = errors.New("audit export configuration is invalid")

// Config is the validated operator configuration.
type Config struct {
	OTLPEndpoint    string
	OTLPProtocol    observability.LogsProtocol
	OTLPHeadersFile string

	WebhookURL      string
	WebhookKeysFile string
	EgressAllowlist []string

	// AuditEvents exports centry.audit_events as well as the security audit.
	AuditEvents bool
	// AuditEventsFromBeginning replays the existing audit_events table on the
	// first run.
	AuditEventsFromBeginning bool

	BatchSize            int
	Interval             time.Duration
	MaxPermanentAttempts int
	MetricsAddr          string
}

// Enabled reports whether any sink is configured.
func (c Config) Enabled() bool { return c.OTLPEndpoint != "" || c.WebhookURL != "" }

// EnvNames lists every variable, for "is anything configured" checks and docs.
var EnvNames = []string{
	EnvOTLPEndpoint, EnvOTLPProtocol, EnvOTLPHeadersFile, EnvWebhookURL, EnvWebhookKeysFile,
	EnvEgressAllowlist, EnvAuditEvents, EnvAuditEventsFrom, EnvBatchSize, EnvInterval,
	EnvMaxPermanent, EnvMetricsAddr,
}

// ConfigFromEnv reads and validates the configuration. With neither endpoint
// set, export is off and no other variable is read: nothing dials anything.
func ConfigFromEnv(lookup func(string) (string, bool)) (Config, error) {
	get := func(name string) string {
		v, _ := lookup(name)
		return strings.TrimSpace(v)
	}
	cfg := Config{
		OTLPEndpoint: get(EnvOTLPEndpoint),
		WebhookURL:   get(EnvWebhookURL),
	}
	if !cfg.Enabled() {
		return cfg, nil
	}
	fail := func(name, why string) (Config, error) {
		return Config{}, fmt.Errorf("%w: %s %s", ErrInvalidConfig, name, why)
	}

	if cfg.OTLPEndpoint != "" {
		proto := get(EnvOTLPProtocol)
		if proto == "" {
			proto = defaultOTLPProtocol
		}
		switch observability.LogsProtocol(proto) {
		case observability.LogsProtocolHTTP, observability.LogsProtocolGRPC:
			cfg.OTLPProtocol = observability.LogsProtocol(proto)
		default:
			return fail(EnvOTLPProtocol, "must be http or grpc")
		}
		if cfg.OTLPProtocol == observability.LogsProtocolHTTP {
			u, err := url.Parse(cfg.OTLPEndpoint)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return fail(EnvOTLPEndpoint, "must be an http(s) base URL for the http protocol")
			}
		}
		cfg.OTLPHeadersFile = get(EnvOTLPHeadersFile)
	} else if get(EnvOTLPHeadersFile) != "" || get(EnvOTLPProtocol) != "" {
		return fail(EnvOTLPEndpoint, "is required when an OTLP protocol or headers file is set")
	}

	if cfg.WebhookURL != "" {
		u, err := url.Parse(cfg.WebhookURL)
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fail(EnvWebhookURL, "must be an http(s) URL without credentials")
		}
		cfg.WebhookKeysFile = get(EnvWebhookKeysFile)
		if cfg.WebhookKeysFile == "" {
			return fail(EnvWebhookKeysFile, "is required with a webhook URL")
		}
		if !strings.HasPrefix(cfg.WebhookKeysFile, "/") {
			return fail(EnvWebhookKeysFile, "must be an absolute path")
		}
		cfg.EgressAllowlist = splitList(get(EnvEgressAllowlist))
		// Plain http carries audit evidence in clear. It is accepted only
		// where the operator has named the destination in the allowlist, which
		// is also the only way to reach a private address.
		if u.Scheme == "http" && len(cfg.EgressAllowlist) == 0 {
			return fail(EnvWebhookURL, "must be https unless "+EnvEgressAllowlist+" names the receiver")
		}
	} else if get(EnvWebhookKeysFile) != "" {
		return fail(EnvWebhookURL, "is required when a webhook key file is set")
	}

	cfg.AuditEvents = true
	if v := get(EnvAuditEvents); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fail(EnvAuditEvents, "must be true or false")
		}
		cfg.AuditEvents = b
	}
	switch from := get(EnvAuditEventsFrom); from {
	case "", defaultAuditFromMode:
	case "beginning":
		cfg.AuditEventsFromBeginning = true
	default:
		return fail(EnvAuditEventsFrom, "must be now or beginning")
	}
	cfg.BatchSize = DefaultBatchSize
	if v := get(EnvBatchSize); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxBatchSize {
			return fail(EnvBatchSize, fmt.Sprintf("must be 1 to %d", maxBatchSize))
		}
		cfg.BatchSize = n
	}
	cfg.Interval = DefaultInterval
	if v := get(EnvInterval); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second || d > time.Hour {
			return fail(EnvInterval, "must be a duration from 1s to 1h")
		}
		cfg.Interval = d
	}
	cfg.MaxPermanentAttempts = DefaultMaxPermanentAttempts
	if v := get(EnvMaxPermanent); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			return fail(EnvMaxPermanent, "must be 1 to 1000")
		}
		cfg.MaxPermanentAttempts = n
	}
	cfg.MetricsAddr = get(EnvMetricsAddr)
	return cfg, nil
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Service is the running export: the sinks, their metrics and the relays.
type Service struct {
	cfg         Config
	sinks       Fanout
	deadLetters DeadLetterStore
	metrics     *Metrics
	db          DB
	logger      *slog.Logger
	closers     []func(context.Context) error
}

// Deps are the collaborators Build needs.
type Deps struct {
	DB             DB
	Logger         *slog.Logger
	ServiceVersion string
}

// Build constructs the sinks. It reads the key and header files once, so a
// missing file refuses boot instead of surfacing as a stalled export.
func Build(ctx context.Context, cfg Config, deps Deps) (*Service, error) {
	if !cfg.Enabled() {
		return nil, errors.New("audit export is not configured")
	}
	if deps.DB == nil {
		return nil, errors.New("audit export needs the platform database")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	metrics := NewMetrics()
	pipeline := PipelineConfig{
		Metrics:              metrics,
		DeadLetters:          DeadLetterRecorder{DB: deps.DB},
		MaxPermanentAttempts: cfg.MaxPermanentAttempts,
		Logger:               deps.Logger,
	}
	metrics.RegisterDecodeFailures()
	svc := &Service{cfg: cfg, metrics: metrics, db: deps.DB, logger: deps.Logger, deadLetters: pipeline.DeadLetters}
	var sinks Fanout

	if cfg.OTLPEndpoint != "" {
		headers, err := readHeadersFile(cfg.OTLPHeadersFile)
		if err != nil {
			return nil, err
		}
		exporter, err := observability.NewLogsExporter(ctx, observability.LogsConfig{
			ServiceName: "elitea-main", ServiceVersion: deps.ServiceVersion,
			Endpoint: cfg.OTLPEndpoint, Protocol: cfg.OTLPProtocol, Headers: headers,
		})
		if err != nil {
			return nil, fmt.Errorf("audit export: %w", err)
		}
		svc.closers = append(svc.closers, exporter.Shutdown)
		sink, err := NewOTLPSink(exporter)
		if err != nil {
			return nil, err
		}
		sinks = append(sinks, Wrap(sink, pipeline))
	}

	if cfg.WebhookURL != "" {
		keys, err := FileKeySource(cfg.WebhookKeysFile, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrInvalidConfig, EnvWebhookKeysFile, err)
		}
		allowlist, err := egress.ParseAllowlist(cfg.EgressAllowlist)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrInvalidConfig, EnvEgressAllowlist, err)
		}
		guard := egress.New(allowlist)
		client := &http.Client{
			Transport: guard.RoundTripper(),
			Timeout:   DefaultExportTimeout,
			// A redirect could carry the signed body to a destination the
			// allowlist never saw. It is returned as a transient failure.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		sink, err := NewWebhookSink(WebhookConfig{URL: cfg.WebhookURL, Keys: keys, Client: client, BatchSize: 100})
		if err != nil {
			return nil, err
		}
		sinks = append(sinks, Wrap(sink, pipeline))
	}
	svc.sinks = sinks
	return svc, nil
}

func readHeadersFile(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("%w: %s must be an absolute path", ErrInvalidConfig, EnvOTLPHeadersFile)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidConfig, EnvOTLPHeadersFile, err)
	}
	if info.Size() > maxHeadersFileSize {
		return nil, fmt.Errorf("%w: %s is too large", ErrInvalidConfig, EnvOTLPHeadersFile)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidConfig, EnvOTLPHeadersFile, err)
	}
	var headers map[string]string
	if err := json.Unmarshal(raw, &headers); err != nil {
		return nil, fmt.Errorf("%w: %s must be a JSON object of header names to values", ErrInvalidConfig, EnvOTLPHeadersFile)
	}
	return headers, nil
}

// Metrics returns the registry.
func (s *Service) Metrics() *Metrics { return s.metrics }

// SecurityExporter is the exporter to give securityaudit.RelayConfig.
func (s *Service) SecurityExporter() securityaudit.Exporter {
	return SecurityExporter{Sink: s.sinks, DeadLetters: s.deadLetters, Metrics: s.metrics, Logger: s.logger}
}

// Run starts the audit_events relay, the backlog monitor and, when
// ELITEA_AUDIT_EXPORT_METRICS_ADDR is set, the metrics listener. It blocks
// until ctx ends. securityEnabled reports the security audit stream too.
func (s *Service) Run(ctx context.Context, securityEnabled bool) error {
	monitor := Monitor{DB: s.db, Metrics: s.metrics, Logger: s.logger, Security: securityEnabled}
	go monitor.Run(ctx)

	if s.cfg.MetricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", s.metrics.Handler())
		server := &http.Server{Addr: s.cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.logger.Error("auditexport: metrics listener stopped", "addr", s.cfg.MetricsAddr, "err", err)
			}
		}()
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		}()
	}

	if !s.cfg.AuditEvents {
		<-ctx.Done()
		return nil
	}
	relay, err := NewAuditEventsRelay(s.db, AuditEventsConfig{
		Sinks: s.sinks, FromBeginning: s.cfg.AuditEventsFromBeginning,
		Interval: s.cfg.Interval, BatchSize: s.cfg.BatchSize, Logger: s.logger,
	})
	if err != nil {
		return err
	}
	relay.Run(ctx)
	return nil
}

// Close shuts the exporters down.
func (s *Service) Close(ctx context.Context) error {
	var errs []error
	for _, closer := range s.closers {
		errs = append(errs, closer(ctx))
	}
	return errors.Join(errs...)
}
