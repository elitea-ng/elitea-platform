// Package audit reports the proxy's run summaries to elitea-main's
// EgressAuditService (ADR-0034 decision 9: "Main's audit log receives a
// per-turn summary of the hosts reached and the hosts refused").
//
// The call is gRPC over mTLS to elitea-main's private control listener,
// never internal REST (AGENTS.md). The proxy presents its own client
// certificate (one DNS SAN); elitea-main admits only that identity
// (ELITEA_EGRESS_AUDIT_CLIENTS).
//
// Reporting never slows the proxy down: Submit is a non-blocking send into a
// bounded queue, and one goroutine delivers with a short retry. A summary
// that cannot be delivered is dropped, counted
// (egress_proxy_audit_dropped_total) and still in the proxy's own log
// (egress_proxy.run_summary). A report is a cumulative snapshot, so a later
// one supersedes a dropped one for the same run.
package audit

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-egress-proxy/internal/config"
	"github.com/EliteaAI/elitea-platform/services/elitea-egress-proxy/internal/proxy"
)

// queueSize bounds the summaries waiting for delivery.
const queueSize = 1024

// attempts per summary, with retryDelay between them.
const (
	attempts   = 3
	retryDelay = 500 * time.Millisecond
)

// Client is the one call the reporter makes (runtimev1.EgressAuditServiceClient).
type Client interface {
	ReportRunSummary(ctx context.Context, in *runtimev1.ReportRunSummaryRequestV1, opts ...grpc.CallOption) (*runtimev1.ReportRunSummaryResponseV1, error)
}

// Reporter delivers summaries to elitea-main.
type Reporter struct {
	client    Client
	replicaID string
	timeout   time.Duration
	logger    *slog.Logger
	queue     chan *runtimev1.ReportRunSummaryRequestV1
	conn      *grpc.ClientConn

	delivered atomic.Int64
	dropped   atomic.Int64
	wg        sync.WaitGroup
	closeOnce sync.Once
	stop      chan struct{}
}

// Dial builds a reporter over an mTLS connection to cfg.Target. The client
// certificate is read again on every TLS handshake, so a renewed
// certificate is used without a restart.
func Dial(cfg config.AuditConfig, logger *slog.Logger) (*Reporter, error) {
	caPEM, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("audit: read the CA file: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("audit: the CA file holds no certificate")
	}
	// Fail at start, not at the first report, when the pair does not load.
	if _, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile); err != nil {
		return nil, fmt.Errorf("audit: load the client certificate: %w", err)
	}
	serverName := cfg.ServerName
	if serverName == "" {
		host, _, err := net.SplitHostPort(cfg.Target)
		if err != nil {
			return nil, fmt.Errorf("audit: the target is not host:port: %w", err)
		}
		serverName = host
	}
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    roots,
		ServerName: serverName,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			pair, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
			if err != nil {
				return nil, err
			}
			return &pair, nil
		},
	}
	conn, err := grpc.NewClient(cfg.Target, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		return nil, fmt.Errorf("audit: build the client: %w", err)
	}
	reporter := New(runtimev1.NewEgressAuditServiceClient(conn), cfg.ReplicaID, cfg.Timeout, logger)
	reporter.conn = conn
	return reporter, nil
}

// New builds a reporter over client, for tests and for Dial.
func New(client Client, replicaID string, timeout time.Duration, logger *slog.Logger) *Reporter {
	if logger == nil {
		logger = slog.Default()
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if replicaID == "" {
		replicaID, _ = os.Hostname()
	}
	return &Reporter{
		client: client, replicaID: replicaID, timeout: timeout, logger: logger,
		queue: make(chan *runtimev1.ReportRunSummaryRequestV1, queueSize), stop: make(chan struct{}),
	}
}

// Start runs the delivery loop until Close.
func (r *Reporter) Start() {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			select {
			case request := <-r.queue:
				r.deliver(request)
			case <-r.stop:
				return
			}
		}
	}()
}

// Submit queues one summary. It never blocks; a full queue drops it.
func (r *Reporter) Submit(summary proxy.Summary) {
	select {
	case r.queue <- r.request(summary):
	default:
		r.dropped.Add(1)
		r.logger.Warn("egress_proxy.audit_dropped", "execution_id", summary.ExecutionID, "reason", "queue-full")
	}
}

// Close stops the loop, then delivers what is queued until ctx ends (the
// shutdown sweep's replica_shutdown summaries come last).
func (r *Reporter) Close(ctx context.Context) {
	r.closeOnce.Do(func() { close(r.stop) })
	r.wg.Wait()
	for {
		select {
		case request := <-r.queue:
			if ctx.Err() != nil {
				r.dropped.Add(1)
				continue
			}
			r.deliver(request)
		default:
			if r.conn != nil {
				_ = r.conn.Close()
			}
			return
		}
	}
}

// Delivered and Dropped are the counters, for metrics and tests.
func (r *Reporter) Delivered() int64 { return r.delivered.Load() }
func (r *Reporter) Dropped() int64   { return r.dropped.Load() }

func (r *Reporter) deliver(request *runtimev1.ReportRunSummaryRequestV1) {
	var err error
	for attempt := range attempts {
		if attempt > 0 {
			select {
			case <-time.After(retryDelay):
			case <-r.stop:
				// Shutting down: one more try happens in Close's drain.
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
		_, err = r.client.ReportRunSummary(ctx, request)
		cancel()
		if err == nil {
			r.delivered.Add(1)
			return
		}
	}
	r.dropped.Add(1)
	r.logger.Warn("egress_proxy.audit_dropped", "execution_id", request.GetExecutionId(), "reason", "delivery-failed",
		"error", err.Error())
}

func (r *Reporter) request(s proxy.Summary) *runtimev1.ReportRunSummaryRequestV1 {
	request := &runtimev1.ReportRunSummaryRequestV1{
		ReplicaId: r.replicaID, ExecutionId: s.ExecutionID, ProjectId: s.ProjectID,
		Final: s.Final, ReplicaShutdown: s.ReplicaShutdown, TokenExpiresAtUnix: s.TokenExpiresAt.Unix(),
		Connections: s.Connections, BytesUp: s.BytesUp, BytesDown: s.BytesDown, OtherHosts: s.OtherHosts,
	}
	for _, h := range s.HostsReached {
		request.HostsReached = append(request.HostsReached, &runtimev1.EgressHostReachedV1{
			Host: h.Host, Port: uint32(max(h.Port, 0)), Connections: h.Connections, BytesUp: h.BytesUp, BytesDown: h.BytesDown, //nolint:gosec // a port is 0..65535
		})
	}
	for _, h := range s.HostsRefused {
		request.HostsRefused = append(request.HostsRefused, &runtimev1.EgressHostRefusedV1{
			Host: h.Host, Port: uint32(max(h.Port, 0)), Reason: h.Reason, Count: h.Count, //nolint:gosec // a port is 0..65535
		})
	}
	fit(request)
	return request
}

// maxReportBytes keeps one report under elitea-main's control listener
// request bound (64 KiB) with room to spare.
const maxReportBytes = 56 << 10

// fit moves host entries into other_hosts until the report fits: the
// refused hosts go last, so what a run reached stays visible longest.
func fit(request *runtimev1.ReportRunSummaryRequestV1) {
	for proto.Size(request) > maxReportBytes {
		switch {
		case len(request.HostsReached) > len(request.HostsRefused):
			request.HostsReached = request.HostsReached[:len(request.HostsReached)-1]
		case len(request.HostsRefused) > 0:
			request.HostsRefused = request.HostsRefused[:len(request.HostsRefused)-1]
		default:
			return
		}
		request.OtherHosts++
	}
}
