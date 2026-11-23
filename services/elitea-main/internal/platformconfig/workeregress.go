package platformconfig

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SectionWorkerEgress is the platform's worker public egress policy (ADR-0044
// decision 2, #1490). The admin Configuration page writes it; Main reads it
// every time it prepares a worker command, and puts the run's assembled
// allowlist in the signed command (WorkerCommandV1.egress_policy).
//
// `open` (the default) leaves public hosts unrestricted: only the worker's
// SSRF rules apply, and commands carry no policy at all unless the private
// permit below names something. `allowlist` limits a
// run to the platform entries here, its project's entries (the `worker_egress`
// project setting), and the hosts of the toolkits and MCP servers the run
// uses. A governed run (the credential governance profile) is always
// `allowlist`, whatever this says.
//
// `private_permit` (#2950) is the operator's list of private destinations
// (an on-premises Jira, GitLab or Confluence) the worker may reach although
// its SSRF rule refuses private, loopback and CGNAT addresses: a host name or
// *.domain admits what it resolves to, a CIDR block or IP literal the
// addresses inside it. Link-local and metadata addresses stay refused
// whatever it names. It applies in both modes and travels on the signed
// command (WorkerEgressPolicyV1.private_allow); no toolkit setting can add to
// it. Main's own outbound guard can read the same list
// (WorkerEgressSettings.PrivatePermit) for destinations it reaches on the
// operator's behalf.
const SectionWorkerEgress = "worker_egress"

// Field keys of SectionWorkerEgress.
const (
	KeyWorkerEgressMode          = "mode"
	KeyWorkerEgressAllowlist     = "allowlist"
	KeyWorkerEgressPrivatePermit = "private_permit"
)

// WorkerEgressMode is the platform mode.
type WorkerEgressMode string

const (
	WorkerEgressOpen      WorkerEgressMode = "open"
	WorkerEgressAllowlist WorkerEgressMode = "allowlist"
)

// Bounds of the allowlists. A run's assembled policy is at most
// MaxWorkerEgressPolicyEntries (the command contract's bound); the platform
// and project lists are kept well under it so a run's toolkit hosts fit.
const (
	MaxWorkerEgressPlatformEntries = 512
	MaxWorkerEgressProjectEntries  = 256
	MaxWorkerEgressPolicyEntries   = 1024
	// MaxWorkerEgressPrivateEntries bounds the console's private permit and,
	// separately, the deployment's (ELITEA_WORKER_EGRESS_PRIVATE_PERMIT), so
	// their union stays within MaxWorkerEgressPrivatePolicyEntries and each
	// list that saves or starts also dispatches.
	MaxWorkerEgressPrivateEntries = 128
	// MaxWorkerEgressPrivatePolicyEntries is the command contract's bound on
	// WorkerEgressPolicyV1.private_allow.
	MaxWorkerEgressPrivatePolicyEntries = 2 * MaxWorkerEgressPrivateEntries
	MaxWorkerEgressEntryBytes           = 261 // a 253-byte name, a colon and a port
)

// WorkerEgressSettings is the platform section.
type WorkerEgressSettings struct {
	Mode WorkerEgressMode `json:"mode"`
	// Allowlist is the stored list, in the egresslib grammar. It is
	// validated on write; LoadWorkerEgressSettings keeps it as stored, and
	// the command assembly refuses a malformed entry (fail closed).
	Allowlist []string `json:"allowlist"`
	// PrivatePermit is the stored private-destination permit (#2950), in
	// the egresslib grammar, kept as stored like Allowlist: the command
	// assembly refuses a malformed entry.
	PrivatePermit []string `json:"private_permit"`
}

// DefaultWorkerEgressSettings is a deployment that never saved the section.
func DefaultWorkerEgressSettings() WorkerEgressSettings {
	return WorkerEgressSettings{Mode: WorkerEgressOpen, Allowlist: []string{}, PrivatePermit: []string{}}
}

// LoadWorkerEgressSettings reads the section over the defaults. A query error
// answers the defaults and the error; the command assembly refuses to prepare
// a command on that error rather than run a run open by accident. A database
// without the table (one never bootstrapped with the admin schema) never
// saved the section, so it answers the defaults without an error.
func LoadWorkerEgressSettings(ctx context.Context, pool *pgxpool.Pool) (WorkerEgressSettings, error) {
	values, err := Load(ctx, pool, SectionWorkerEgress)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return DefaultWorkerEgressSettings(), nil
	}
	if err != nil {
		return DefaultWorkerEgressSettings(), err
	}
	return WorkerEgressSettingsFromValues(values), nil
}

// WorkerEgressSettingsFromValues is the overlay. An unknown mode is
// `allowlist`: a hand-written row that misspelt the mode narrows, never
// widens. A stored list keeps its entries as written, malformed ones too,
// so the assembly can refuse them.
func WorkerEgressSettingsFromValues(values Values) WorkerEgressSettings {
	settings := DefaultWorkerEgressSettings()
	if raw, present := values[KeyWorkerEgressMode]; present {
		mode, _ := raw.(string)
		switch WorkerEgressMode(strings.TrimSpace(mode)) {
		case WorkerEgressOpen:
			settings.Mode = WorkerEgressOpen
		default:
			settings.Mode = WorkerEgressAllowlist
		}
	}
	if raw, present := values[KeyWorkerEgressAllowlist].([]any); present {
		settings.Allowlist = storedEntries(raw)
	}
	if raw, present := values[KeyWorkerEgressPrivatePermit].([]any); present {
		settings.PrivatePermit = storedEntries(raw)
	}
	return settings
}

// storedEntries keeps a stored list as written. A value that is not a string
// is kept as a value the grammar refuses, so the assembly fails closed on it
// instead of skipping it.
func storedEntries(raw []any) []string {
	entries := make([]string, 0, len(raw))
	for _, item := range raw {
		entry, ok := item.(string)
		if !ok {
			entry = fmt.Sprint(item) + " (not a string)"
		}
		entries = append(entries, entry)
	}
	return entries
}

// ValidateWorkerEgressEntries checks a list an administrator saves, the
// platform's or a project's: every entry parses in the egresslib grammar, is
// not blank, and the list is at most limit long. The error names the entry.
func ValidateWorkerEgressEntries(entries []string, limit int) error {
	if len(entries) > limit {
		return fmt.Errorf("at most %d entries", limit)
	}
	for _, entry := range entries {
		if len(entry) > MaxWorkerEgressEntryBytes {
			return fmt.Errorf("entry %q is too long", entry)
		}
		if _, err := egresslib.ParseEntry(entry); err != nil {
			return err
		}
	}
	return nil
}
