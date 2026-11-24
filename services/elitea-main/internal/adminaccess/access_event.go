package adminaccess

// The security audit event of an administrative access change, until story
// R1.5 (issue 2509) defines its own type `admin.access.changed`.
//
// R1.5 lists the operator command (R0.12a) and the refused SCIM
// deprovisioning (R0.11) as producers. Both write through AppendAccessEvent,
// so R1.5 switches one call site: the Type below. The actions are the change
// kinds R1.5 uses, prefixed `admin.access.`; the reasons carry the source and
// the severity, because the chain has no severity field.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// The actions of an access event.
const (
	AccessActionRoleAssigned = "admin.access.role_assigned"
	AccessActionRoleRemoved  = "admin.access.role_removed"
	// AccessActionRoleSetReset is `admin-role reset`: a built-in role's set
	// put back to the registry set. Always critical.
	AccessActionRoleSetReset = "admin.access.role_set_reset"
	// AccessActionDeprovisionRefused is a SCIM deactivation of the last
	// administrator: the account and its roles stay, its access ended.
	// Always critical.
	AccessActionDeprovisionRefused = "admin.access.deprovision_refused"
)

// The severities an access event records as a reason code.
const (
	SeverityNotice   = "notice"
	SeverityCritical = "critical"
)

// AccessEvent is one access change.
type AccessEvent struct {
	Action string
	// Source is the grant-history source (rolegrants.Source*).
	Source string
	// PrincipalRef is who made the change: "user:<id>", "operator:<label>"
	// or "scim:<client>".
	PrincipalRef string
	// SubjectUserID is the user whose access changed; 0 for a role-set
	// change.
	SubjectUserID int64
	// Role is the role the change is about.
	Role string
	// Before and After are a role's permission sets (a reset), recorded as
	// counts, sha256 digests and the names that changed.
	Before, After []string
	Severity      string
	// Reasons are further reason codes.
	Reasons []string
}

// maxReasonItems is the chain's bound on a list (securityaudit.maxListItems).
const maxReasonItems = 32

// AppendAccessEvent writes the event in tx and returns the chained record.
// An error means the caller rolls back: the change does not happen without its
// evidence.
func AppendAccessEvent(ctx context.Context, tx pgx.Tx, source string, event AccessEvent) (securityaudit.Record, error) {
	appender, err := securityaudit.NewPostgresAppender(source)
	if err != nil {
		return securityaudit.Record{}, err
	}
	severity := event.Severity
	if severity == "" {
		severity = SeverityNotice
	}
	reasons := []string{"source:" + event.Source, "severity:" + severity}
	if event.Role != "" {
		reasons = append(reasons, reasonCode("role:", event.Role))
	}
	reasons = append(reasons, event.Reasons...)
	if event.Before != nil || event.After != nil {
		reasons = append(reasons,
			"before_count:"+strconv.Itoa(len(event.Before)), "before:"+setDigest(event.Before),
			"after_count:"+strconv.Itoa(len(event.After)), "after:"+setDigest(event.After))
		added, removed := setDifference(event.Before, event.After)
		for _, name := range added {
			reasons = append(reasons, reasonCode("added:", name))
		}
		for _, name := range removed {
			reasons = append(reasons, reasonCode("removed:", name))
		}
	}
	if len(reasons) > maxReasonItems {
		// The digests above say exactly what the sets were; the names are a
		// convenience, so a longer list is cut rather than refused.
		reasons = append(reasons[:maxReasonItems-1], "names:truncated")
	}
	securityEvent := securityaudit.SecurityEvent{
		// R1.5 (issue 2509) switches this to its admin.access.changed type.
		Type:         securityaudit.TypeGrantChanged,
		Action:       event.Action,
		Effect:       securityaudit.EffectAllow,
		Reasons:      reasons,
		PrincipalRef: event.PrincipalRef,
	}
	if event.Action == AccessActionDeprovisionRefused {
		securityEvent.Effect = securityaudit.EffectDeny
	}
	if event.SubjectUserID > 0 {
		securityEvent.ResourceType, securityEvent.ResourceID = "user", strconv.FormatInt(event.SubjectUserID, 10)
	} else if event.Role != "" && securityaudit.AcceptsString(event.Role) {
		securityEvent.ResourceType, securityEvent.ResourceID = "role", event.Role
	}
	record, err := appender.AppendRecord(ctx, tx, securityEvent)
	if err != nil {
		return securityaudit.Record{}, fmt.Errorf("append the access event: %w", err)
	}
	return record, nil
}

// reasonShape is the chain's reason-code pattern (securityaudit reasonPattern).
var reasonShape = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]{0,127}$`)

// reasonCode is prefix+value, or prefix and a digest of the value when the
// value is not reason-code shaped (a custom role named "Ops Lead"): the event
// is never refused for a name.
func reasonCode(prefix, value string) string {
	if code := prefix + value; reasonShape.MatchString(code) {
		return code
	}
	sum := sha256.Sum256([]byte(value))
	return prefix + "sha256:" + hex.EncodeToString(sum[:16])
}

// setDigest is "sha256:<hex>" of the sorted names, one per line.
func setDigest(names []string) string {
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func setDifference(before, after []string) (added, removed []string) {
	for _, name := range after {
		if !slices.Contains(before, name) {
			added = append(added, name)
		}
	}
	for _, name := range before {
		if !slices.Contains(after, name) {
			removed = append(removed, name)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	return added, removed
}
