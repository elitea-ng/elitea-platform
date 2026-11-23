package workeregress

import (
	"fmt"
	"strings"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// OperatorPermitEnv is the deployment's own private permit (#2950): private
// destinations every run may reach, in the egresslib grammar, comma
// separated. It is the platform section's `private_permit` set from the
// deployment (a chart value) instead of the admin console, so an operator can
// name the on-premises hosts before an upgrade rolls out. The two lists are
// merged. Like the section, it is operator configuration that no project
// setting or toolkit URL can widen.
const OperatorPermitEnv = "ELITEA_WORKER_EGRESS_PRIVATE_PERMIT"

// ParseOperatorPermit reads OperatorPermitEnv's value. Blank entries (a
// trailing comma) are skipped; a malformed entry is an error naming it, so a
// typo fails the start instead of every run.
func ParseOperatorPermit(value string) ([]string, error) {
	var entries []string
	for _, raw := range strings.Split(value, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if _, err := egresslib.ParseEntry(raw); err != nil {
			return nil, fmt.Errorf("%s: entry %s: %w", OperatorPermitEnv, quoteEntry(raw), err)
		}
		entries = append(entries, raw)
	}
	// Each operator list has half the command's bound, so the merged permit
	// always fits and a list that starts Main never refuses runs later.
	if len(entries) > platformconfig.MaxWorkerEgressPrivateEntries {
		return nil, fmt.Errorf("%s: at most %d entries", OperatorPermitEnv, platformconfig.MaxWorkerEgressPrivateEntries)
	}
	return entries, nil
}

// WithOperatorPermit merges the deployment's private permit
// (OperatorPermitEnv, already parsed) into every command's permit.
func (r *Resolver) WithOperatorPermit(entries []string) *Resolver {
	r.operator = append([]string(nil), entries...)
	return r
}
