package spi

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// DefaultAdminAddr is where the host's admin port listens when
// <PREFIX>ADMIN_ADDR is not set.
const DefaultAdminAddr = ":9090"

// AdminHandler serves the admin port: /healthz (the process is up) and
// /readyz (every readiness check passes — Server.Readiness, the same checks
// the provider service's Health reports). Plain HTTP and no mTLS, like every
// other service's health port: the kubelet and the container probe present
// no client certificate, and the gRPC listener requires one, so a pod whose
// readiness is false (a lost owner claim, #1359) could otherwise never leave
// the Service. /metrics serves the capacity gauges (MetricsText). Nothing
// else is served here.
func AdminHandler(server *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		ready, checks := server.Readiness(r.Context())
		if !ready {
			var failing []string
			for name, ok := range checks {
				if !ok {
					failing = append(failing, name)
				}
			}
			sort.Strings(failing)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "not ready: "+strings.Join(failing, ",")+"\n")
			return
		}
		_, _ = io.WriteString(w, "ready\n")
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(w, server.MetricsText())
	})
	return mux
}

// The capacity gauges an autoscaler reads. An invocation runs on the replica
// that accepted it, start to end, and nothing queues it anywhere else, so the
// load of a replica is its own in-flight count. A Prometheus scaler divides
// the sum over the replicas by the slots of one.
const (
	MetricInvocationsInFlight = "elitea_subapp_invocations_in_flight"
	MetricInvocationSlots     = "elitea_subapp_invocation_slots"
)

// MetricsText is the admin port's /metrics body, in the Prometheus text
// format: the in-flight gauge and the slot count Slots reports as total.
func (s *Server) MetricsText() string {
	app := strconv.Quote(s.app.Name)
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP %s Invocations this replica is running now.\n# TYPE %s gauge\n%s{app=%s} %d\n",
		MetricInvocationsInFlight, MetricInvocationsInFlight, MetricInvocationsInFlight, app, s.manager.InFlight())
	fmt.Fprintf(&b, "# HELP %s Invocations one replica runs at once: <PREFIX>MAX_PARALLEL_WORKERS, the total its slots report.\n# TYPE %s gauge\n%s{app=%s} %d\n",
		MetricInvocationSlots, MetricInvocationSlots, MetricInvocationSlots, app, s.settings.MaxParallelWorkers)
	return b.String()
}
