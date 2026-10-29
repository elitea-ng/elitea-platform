package egressaudit

import "time"

// The admin view of the hosts runs reached (ADR-0044 decision 2): read from
// the same rows the merger audits, so an operator can seed a worker egress
// allowlist from what agents actually used before switching it on.

// Source selects whose rows a hosts query reads.
type Source string

const (
	// SourceWorker is the worker's own HTTP clients: what a worker egress
	// allowlist governs. The default.
	SourceWorker Source = "worker"
	// SourceProxy is the egress proxy's replicas (sandbox and code runs).
	SourceProxy Source = "proxy"
	// SourceAll is both.
	SourceAll Source = "all"
)

// Bounds of a hosts query.
const (
	DefaultHostsDays = 30
	MaxHostsDays     = 90
	MaxHostsRows     = 5000
)

// HostsQuery is one admin query.
type HostsQuery struct {
	Since  time.Time
	Source Source
	// ProjectID narrows to one project; zero is every project.
	ProjectID int64
	// Limit bounds each of the two lists; the report says when it cut one.
	Limit int
}

// ReachedHost is one (project, agent, host, port) the runs reached.
type ReachedHost struct {
	ProjectID int64
	// AgentID is the agent (application) of the runs; nil for runs of no
	// agent (an ad hoc chat, a direct toolkit call, an index run).
	AgentID     *int64
	Host        string
	Port        int
	Connections int64
	Executions  int64
	LastSeen    time.Time
}

// RefusedHost is one (project, agent, host, port, reason) refused.
type RefusedHost struct {
	ProjectID  int64
	AgentID    *int64
	Host       string
	Port       int
	Reason     string
	Count      int64
	Executions int64
	LastSeen   time.Time
}

// HostsReport is the answer to a HostsQuery.
type HostsReport struct {
	Reached          []ReachedHost
	Refused          []RefusedHost
	ReachedTruncated bool
	RefusedTruncated bool
}
