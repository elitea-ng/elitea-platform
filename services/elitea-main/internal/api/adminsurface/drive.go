package adminsurface

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// Route is one mounted (method, pattern).
type Route struct {
	Method  string
	Pattern string
}

func (r Route) String() string { return r.Method + " " + r.Pattern }

// Kind says what probing learned about one administration-mode check.
type Kind string

const (
	// KindAnyOf: each name of Permissions alone satisfies the check.
	KindAnyOf Kind = "any_of"
	// KindAllOf: the check is satisfied only when every name of Permissions is
	// held.
	KindAllOf Kind = "all_of"
	// KindWholeSet: the outcome depends on the whole answer (a listing of the
	// caller's own permissions, "holds any administration permission"). The
	// check names no single permission.
	KindWholeSet Kind = "whole_set"
	// KindInvisible: no registered name changes the outcome. The check names a
	// permission outside the registry, or its effect cannot be seen in this
	// router.
	KindInvisible Kind = "invisible"
	// KindTimedOut: a probe request did not answer within Options.Timeout.
	// Drive reports the route as Unreached with TimedOut set instead of
	// keeping a check it could not read.
	KindTimedOut Kind = "timed_out"
)

// Check is one administration-mode resolution of a route and what it asked.
type Check struct {
	// Index is the order of this resolution among the request's
	// administration-mode resolutions, from 0.
	Index int
	// Caller is the function that resolved; Gate says whether it is a route
	// gate (true) or a check in a handler or another middleware (false).
	Caller string
	Gate   bool
	Kind   Kind
	// Permissions are the registered names the check accepts (KindAnyOf) or
	// requires (KindAllOf). Empty for the other kinds.
	Permissions []string
}

// Entry is one route of the administration surface, as one variant drove it.
type Entry struct {
	Route
	// Prefix is the mount prefix: the first path segment below /api/v2
	// ("/admin", "/elitea_core", …), or the first segment of a route outside it.
	Prefix string
	// Variant names the variant that drove this entry; "" is the default one.
	Variant string
	// Body is the request body that drove this entry.
	Body   string
	Checks []Check
}

// Variant is one way to drive a route: a request body (an `action` of a
// multiplexed route), the path placeholders that select what the handler
// reads (a Configuration section), and what a check that the default request
// never reaches needs: the grants of a gate in another mode, a credential
// other than the bearer token, an open maintenance window.
type Variant struct {
	Name   string
	Body   string
	Params map[string]string
	// Query is the request's query string, without the "?": what a handler
	// reads to decide (the `type` filter of a configurations list, which the
	// central settings branch decides on).
	Query string
	// OtherModes, when not nil, replaces Options.OtherModes for this variant:
	// the grants of a gate in another mode in front of an in-handler
	// administration check (the Marketplace moderation check sits behind
	// the project's agent list gate).
	OtherModes []string
	// Authenticate, when not nil, replaces Options.Authenticate: a check that
	// only another credential reaches (the token mint asks the
	// administration mode of a native device session only).
	Authenticate func(*http.Request)
	// Maintenance drives the variant with a maintenance window open. The
	// router must read the window through MaintenanceState (ComposedRouter
	// does), since no database holds one.
	Maintenance bool
	// Elevation, when set, drives the variant as a Form browser session of
	// the synthetic principal with no standing set, whose every
	// administration-mode resolution reports the grant as given by one
	// active elevation of this level and path (ADR-0059 R2.8a). The
	// variant's Authenticate is replaced by AuthenticateElevatedSession.
	Elevation *Elevation
}

// Elevation is the elevation an elevated variant resolves under.
type Elevation struct {
	Level, Path string
}

// ElevationID is the id of the elevation of an elevated variant.
const ElevationID = "0b7c2d4e-0000-4000-8000-00000000e1e7"

func (v Variant) query() string {
	if v.Query == "" {
		return ""
	}
	return "?" + v.Query
}

type maintenanceKey struct{}

// MaintenanceState answers an open window for a request Drive sent with
// Variant.Maintenance, and a closed one for every other request. It is the
// api.RouterConfig.MaintenanceState of ComposedRouter.
func MaintenanceState(ctx context.Context) (platformconfig.Maintenance, error) {
	if open, _ := ctx.Value(maintenanceKey{}).(bool); open {
		return platformconfig.Maintenance{Enabled: true, Title: MaintenanceTitle}, nil
	}
	return platformconfig.Maintenance{}, nil
}

// MaintenanceTitle is the title of the window MaintenanceState opens.
const MaintenanceTitle = "adminsurface window"

// Unreached is a route Drive could not place in the surface, with the status
// it answered:
//
//   - Modes empty: it made no permission resolution at all.
//   - Modes set (recorded only for an IsAdministrationRoute route): it
//     resolved only those modes, never the administration mode.
//   - Unstable: it answered the same request in two different ways, so nothing
//     probing learns can be trusted.
//   - TimedOut: a request did not answer within Options.Timeout.
type Unreached struct {
	Route
	Variant  string
	Status   int
	Answer   string
	Modes    []string
	Unstable bool
	TimedOut bool
}

// Result is what Drive found.
type Result struct {
	Surface   []Entry
	Unreached []Unreached
	// Driven counts the (route, body) pairs sent.
	Driven int
	// Orphans are resolutions that reached the resolver outside a request
	// Drive sent. A check there is invisible to the probe, so a test fails on
	// any.
	Orphans []Call
}

// Options steer Drive.
type Options struct {
	// Registry is the universe of administration-mode names probed. Required.
	Registry []string
	// OtherModes is granted in every resolution of a mode other than
	// administration, so a project-mode gate in front of an in-handler
	// administration check does not hide it.
	OtherModes []string
	// Variants lists the ways to drive a route, keyed by Route.String(). A
	// route with no entry is driven once, with an empty JSON object for a
	// method that carries a body. A multiplexed route (an `action` in the
	// body) lists one variant per action. A variant with an empty Body takes
	// the default body.
	Variants map[string][]Variant
	// Params overrides a path placeholder by name. The defaults: {mode} is
	// "administration", every other placeholder is "1", a trailing * is "x".
	Params map[string]string
	// Authenticate adds the synthetic principal's credential to a request.
	Authenticate func(*http.Request)
	// Timeout bounds one request. Zero means ten seconds, far above the
	// milliseconds a request of the composed router takes, so that only a
	// handler that waits on something never comes back reaches it. A request
	// that does is reported, never compared.
	Timeout time.Duration
}

// Drive sends every route to handler and returns the administration surface.
// handler must answer its resolutions through resolver.
func Drive(handler http.Handler, resolver *Resolver, routes []Route, opts Options) Result {
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Second
	}
	d := driver{handler: handler, opts: opts}

	type job struct {
		route   Route
		variant Variant
	}
	var jobs []job
	for _, route := range routes {
		variants := opts.Variants[route.String()]
		if len(variants) == 0 {
			variants = []Variant{{}}
		}
		for _, variant := range variants {
			if variant.Body == "" {
				variant.Body = defaultBody(route.Method)
			}
			jobs = append(jobs, job{route, variant})
		}
	}

	var (
		mu     sync.Mutex
		result = Result{Driven: len(jobs)}
		next   = make(chan job)
		wg     sync.WaitGroup
	)
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range next {
				entry, unreached := d.drive(j.route, j.variant)
				mu.Lock()
				if entry != nil {
					result.Surface = append(result.Surface, *entry)
				}
				if unreached != nil {
					result.Unreached = append(result.Unreached, *unreached)
				}
				mu.Unlock()
			}
		}()
	}
	for _, j := range jobs {
		next <- j
	}
	close(next)
	wg.Wait()

	sort.Slice(result.Surface, func(i, j int) bool {
		a, b := result.Surface[i], result.Surface[j]
		if a.Pattern != b.Pattern {
			return a.Pattern < b.Pattern
		}
		if a.Method != b.Method {
			return a.Method < b.Method
		}
		return a.Variant < b.Variant
	})
	sort.Slice(result.Unreached, func(i, j int) bool {
		a, b := result.Unreached[i], result.Unreached[j]
		if a.String() != b.String() {
			return a.String() < b.String()
		}
		return a.Variant < b.Variant
	})
	result.Orphans = resolver.Orphans()
	return result
}

// Answer is what one request answered.
type Answer struct {
	Status int
	Body   string
	// Calls are the permission resolutions the request made.
	Calls []Call
	// Flagged: the admin audit row of the request was marked for the
	// elevation review (ADR-0059 R2.8a).
	Flagged bool
	// Audited are the admin audit rows the request wrote, through the
	// router's AuditRecorder (ComposedRouter keeps them per request). The
	// coverage test of ADR-0059 R1.7 reads them.
	Audited []audit.Event
}

// Maintenance reports whether the answer is the maintenance gate's refusal:
// 503 with `"maintenance": true` (middleware writeMaintenance).
func (a Answer) Maintenance() bool {
	return a.Status == http.StatusServiceUnavailable && strings.Contains(a.Body, `"maintenance":true`)
}

// Send sends one request of route as variant drives it, as a caller who holds
// exactly grant in the administration mode and opts.OtherModes (or the
// variant's) in every other mode. It probes nothing: it is the request a
// holder of that set makes. A request that does not answer within
// opts.Timeout answers status -1.
func Send(handler http.Handler, route Route, variant Variant, opts Options, grant []string) Answer {
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Second
	}
	if variant.Body == "" {
		variant.Body = defaultBody(route.Method)
	}
	d := driver{handler: handler, opts: opts}
	out := d.run(route, variant, func(int) []string { return grant })
	return Answer{Status: out.status, Body: out.body, Calls: out.calls, Flagged: out.flagged, Audited: out.audited}
}

func defaultBody(method string) string {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return "{}"
	}
	return ""
}

type driver struct {
	handler    http.Handler
	opts       Options
	ignoreBody bool
}

// outcome is what one request did: its answer and every resolution it made.
type outcome struct {
	status  int
	body    string
	calls   []Call
	flagged bool
	audited []audit.Event
}

// The statuses of a request that did not answer normally.
const (
	statusTimedOut = -1
	statusPanicked = -2
)

func (o outcome) timedOut() bool { return o.status == statusTimedOut }

func (o outcome) admin() []Call {
	var out []Call
	for _, c := range o.calls {
		if c.Mode == auth.PermissionModeAdministration {
			out = append(out, c)
		}
	}
	return out
}

func (o outcome) equal(p outcome) bool {
	return o.status == p.status && o.body == p.body && slices.Equal(o.calls, p.calls)
}

func (d driver) drive(route Route, variant Variant) (*Entry, *Unreached) {
	all := d.opts.Registry
	everything := func(int) []string { return all }
	full := d.run(route, variant, everything)
	if full.timedOut() {
		return nil, &Unreached{Route: route, Variant: variant.Name, Status: full.status, TimedOut: true}
	}
	if len(full.calls) == 0 {
		return nil, &Unreached{Route: route, Variant: variant.Name, Status: full.status, Answer: truncate(full.body)}
	}
	adminCalls := full.admin()
	if len(adminCalls) == 0 {
		// The pass that grants nothing can only stop earlier; it never
		// resolves the administration mode where granting everything does not.
		// A route whose path names the administration plane and that decides
		// in another mode only is reported, so that a wrong gate cannot read
		// as "outside".
		if claimsAdministration(route) {
			return nil, &Unreached{Route: route, Variant: variant.Name, Status: full.status,
				Answer: truncate(full.body), Modes: modes(full.calls)}
		}
		return nil, nil
	}
	// The answer must not change between two identical requests, or no probe
	// below means anything. A body that carries a per-request value (a support
	// reference, a time) is compared by status and resolutions only.
	if again := d.run(route, variant, everything); !again.equal(full) {
		d.ignoreBody = true
		full = d.run(route, variant, everything)
		again = d.run(route, variant, everything)
		if full.timedOut() || again.timedOut() {
			return nil, &Unreached{Route: route, Variant: variant.Name, Status: statusTimedOut, TimedOut: true}
		}
		if !again.equal(full) {
			return nil, &Unreached{Route: route, Variant: variant.Name, Status: full.status, Unstable: true,
				Answer: "answers differently to the same request, so no probe can be read"}
		}
	}

	entry := &Entry{Route: route, Prefix: mountPrefix(route.Pattern), Variant: variant.Name, Body: variant.Body}
	for i, call := range adminCalls {
		check := d.probe(route, variant, full, i, call)
		if check.Kind == KindTimedOut {
			return nil, &Unreached{Route: route, Variant: variant.Name, Status: statusTimedOut, TimedOut: true}
		}
		entry.Checks = append(entry.Checks, check)
	}
	return entry, nil
}

func modes(calls []Call) []string {
	var out []string
	for _, c := range calls {
		if !slices.Contains(out, c.Mode) {
			out = append(out, c.Mode)
		}
	}
	slices.Sort(out)
	return out
}

// probe learns what the i-th administration-mode resolution asks for. A probe
// request that times out ends it with KindTimedOut: its answer is no evidence.
func (d driver) probe(route Route, variant Variant, full outcome, i int, call Call) (check Check) {
	all := d.opts.Registry
	type timedOut struct{}
	defer func() {
		if p := recover(); p != nil {
			if _, ok := p.(timedOut); !ok {
				panic(p)
			}
			check.Kind, check.Permissions = KindTimedOut, nil
		}
	}()
	at := func(probe []string) outcome {
		out := d.run(route, variant, func(n int) []string {
			if n == i {
				return probe
			}
			return all
		})
		if out.timedOut() {
			panic(timedOut{})
		}
		return out
	}
	check = Check{Index: i, Caller: call.Caller, Gate: call.Gate}

	// Granting nothing must change the outcome, or no name can be said to
	// satisfy the check: a check that only an unregistered name passes answers
	// every probe below exactly as it answers the full grant.
	if at(nil).equal(full) {
		check.Kind = KindInvisible
		return check
	}
	var anyOf []string
	for _, name := range all {
		if at([]string{name}).equal(full) {
			anyOf = append(anyOf, name)
		}
	}
	// A check that every registered name satisfies alone asks whether the
	// caller holds ANY administration permission (the token mint of a
	// native session, the maintenance gate in front of a route an
	// administration gate decides): it reads the whole answer and names no
	// permission.
	if len(anyOf) > 0 && len(anyOf) == len(all) {
		check.Kind = KindWholeSet
		return check
	}
	if len(anyOf) > 0 {
		check.Kind, check.Permissions = KindAnyOf, anyOf
		return check
	}
	var required []string
	for _, name := range all {
		without := slices.DeleteFunc(slices.Clone(all), func(n string) bool { return n == name })
		if !at(without).equal(full) {
			required = append(required, name)
		}
	}
	if len(required) > 0 && len(required) < len(all) && at(required).equal(full) {
		check.Kind, check.Permissions = KindAllOf, required
		return check
	}
	check.Kind = KindWholeSet
	return check
}

func (d driver) run(route Route, variant Variant, grant func(int) []string) outcome {
	others := d.opts.OtherModes
	if variant.OtherModes != nil {
		others = variant.OtherModes
	}
	rec := &recorder{grant: grant, others: others, elevation: variant.Elevation}
	base := context.WithValue(context.Background(), recorderKey{}, rec)
	if variant.Maintenance {
		base = context.WithValue(base, maintenanceKey{}, true)
	}
	ctx, cancel := context.WithTimeout(base, d.opts.Timeout)
	defer cancel()

	req := httptest.NewRequestWithContext(ctx, route.Method, d.path(route.Pattern, variant.Params)+variant.query(),
		strings.NewReader(variant.Body))
	if variant.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	switch {
	case variant.Elevation != nil:
		AuthenticateElevatedSession(req)
	case variant.Authenticate != nil:
		variant.Authenticate(req)
	case d.opts.Authenticate != nil:
		d.opts.Authenticate(req)
	}

	w := httptest.NewRecorder()
	done := make(chan string, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- fmt.Sprintf("panic: %v", p)
				return
			}
			done <- ""
		}()
		d.handler.ServeHTTP(w, req)
	}()
	var panicked string
	select {
	case panicked = <-done:
	case <-time.After(d.opts.Timeout + time.Second):
		return outcome{status: statusTimedOut, calls: rec.snapshot()}
	}
	// A handler that gave up on the deadline answered for the deadline, not
	// for the grant: that answer is no evidence either.
	if ctx.Err() != nil {
		return outcome{status: statusTimedOut, calls: rec.snapshot()}
	}
	out := outcome{status: w.Code, body: w.Body.String(), calls: rec.snapshot(), flagged: rec.isFlagged(),
		audited: rec.auditedEvents()}
	if panicked != "" {
		// A panic the router did not recover is an answer like any other:
		// the stability check in drive catches one that is not repeatable.
		out.status, out.body = statusPanicked, firstLine(panicked)
	}
	if d.ignoreBody {
		out.body = ""
	}
	return out
}

// RequestPath answers the path Drive and Send request for pattern: params
// first, then {mode} as "administration", every other placeholder "1" and a
// trailing * "x".
func RequestPath(pattern string, params map[string]string) string {
	return driver{}.path(pattern, params)
}

var placeholder = regexp.MustCompile(`\{([^}:]+)(:[^}]*)?\}`)

func (d driver) path(pattern string, params map[string]string) string {
	path := placeholder.ReplaceAllStringFunc(pattern, func(m string) string {
		name := placeholder.FindStringSubmatch(m)[1]
		if v, ok := params[name]; ok {
			return v
		}
		if v, ok := d.opts.Params[name]; ok {
			return v
		}
		if name == "mode" {
			return auth.PermissionModeAdministration
		}
		return "1"
	})
	// A "/*/" inside a pattern is a sub-router mounted at "/" (chi writes the
	// mount's wildcard into the walked pattern); the request path has no
	// segment there. A gate inside that sub-router is reached only on the real
	// path, so drive that (adminaccess.RoutePattern does the same).
	for strings.Contains(path, "/*/") {
		path = strings.ReplaceAll(path, "/*/", "/")
	}
	if strings.HasSuffix(path, "*") {
		path = strings.TrimSuffix(path, "*") + "x"
	}
	return path
}

// mountPrefix answers the first segment below /api/v2, or the first segment
// of a route outside it.
func mountPrefix(pattern string) string {
	rest := strings.TrimPrefix(pattern, "/api/v2")
	rest = strings.TrimPrefix(rest, "/")
	if i := strings.Index(rest, "/"); i >= 0 {
		rest = rest[:i]
	}
	return "/" + rest
}

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
