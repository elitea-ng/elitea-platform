// Package auditexport sends audit evidence to a customer's SIEM (ADR-0042
// decision 3, issue #1494).
//
// Three streams feed it: the security audit outbox
// (internal/securityaudit, whose relay offers each batch to an
// securityaudit.Exporter and keeps the rows pending until the exporter
// returns nil), centry.audit_events (the HTTP access audit, tailed by
// AuditEventsRelay in commit-safe order, with a persisted position per sink)
// and elitea_runtime.run_steps (the step records of a run, the same relay on
// stream run_steps). All are mapped to one versioned envelope, elitea.audit.v1 (libs/jsonschema/audit/v1), and sent
// to every configured Sink: an OTLP logs collector or a signed webhook.
//
// Delivery is at least once. A sink is acknowledged only after the receiver
// accepted the batch, so killing and restoring a receiver loses nothing; a
// receiver deduplicates on event_id. A healthy sink is not sent an event
// again because another sink failed; duplicates come only from a crash or a
// relay handover between a send and its acknowledgement. Export is asynchronous: it runs in the
// relays' goroutines and never blocks the request or run that produced the
// event.
package auditexport

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// SchemaVersion is the envelope's "schema" value.
const SchemaVersion = "elitea.audit.v1"

// Source names the evidence stream of an event.
type Source string

const (
	SourceSecurityAudit Source = "security_audit"
	// SourceAdminAudit is centry.audit_events, the HTTP access audit behind
	// the admin Audit Trail.
	SourceAdminAudit Source = "admin_audit"
	// SourceRunStep is elitea_runtime.run_steps, the step records of a run
	// (#1499), mapped by FromRunStep.
	SourceRunStep Source = "run_step"
	// SourceApproval and SourceSuspension are reserved for the ADR-0042
	// evidence added later; no mapper produces them yet.
	SourceApproval   Source = "approval"
	SourceSuspension Source = "suspension"
)

// Outcome values.
const (
	OutcomeAllow   = "allow"
	OutcomeDeny    = "deny"
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
	OutcomeUnknown = "unknown"
)

// Event is one elitea.audit.v1 envelope. The JSON field order below is the
// order a receiver sees; fields with no value are omitted, never null.
type Event struct {
	Schema      string       `json:"schema"`
	EventID     string       `json:"event_id"`
	Time        string       `json:"time"`
	Source      Source       `json:"source"`
	EventType   string       `json:"event_type"`
	OCSF        *OCSF        `json:"ocsf,omitempty"`
	Tenant      Tenant       `json:"tenant"`
	Actor       Actor        `json:"actor"`
	Action      string       `json:"action"`
	Target      *Target      `json:"target,omitempty"`
	Outcome     string       `json:"outcome"`
	Correlation *Correlation `json:"correlation,omitempty"`
	Chain       *Chain       `json:"chain,omitempty"`
	Details     any          `json:"details"`

	// ref identifies the source row ("security_audit:42"), for dead letters
	// and logs. It is not part of the envelope.
	ref string
}

// Ref is the stream-qualified source row of the event.
func (e Event) Ref() string { return e.ref }

// OCSF is the OCSF class, when one fits.
type OCSF struct {
	ClassUID    int `json:"class_uid"`
	CategoryUID int `json:"category_uid"`
}

// Tenant is the event's tenant project.
type Tenant struct {
	ProjectID *int64 `json:"project_id"`
}

// Actor is who acted.
type Actor struct {
	Type     string   `json:"type"`
	ID       string   `json:"id"`
	Email    string   `json:"email,omitempty"`
	Chain    []string `json:"chain,omitempty"`
	Fidelity string   `json:"fidelity,omitempty"`
}

// Target is what the action acted on.
type Target struct {
	Type string `json:"type,omitempty"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// Correlation ties the event to a trace or request.
type Correlation struct {
	TraceID       string `json:"trace_id,omitempty"`
	SpanID        string `json:"span_id,omitempty"`
	ParentSpanID  string `json:"parent_span_id,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// Chain carries the tamper-evident chain fields.
type Chain struct {
	Partition string `json:"partition"`
	Sequence  int64  `json:"sequence"`
	PrevHash  string `json:"prev_hash"`
	Hash      string `json:"hash"`
}

// SecurityAuditDetails is details for source security_audit.
type SecurityAuditDetails struct {
	Reasons              []string `json:"reasons"`
	CallingApplicationID string   `json:"calling_application_id,omitempty"`
	AuthenticationRef    string   `json:"authentication_ref,omitempty"`
	AgentBindingID       string   `json:"agent_binding_id,omitempty"`
	AuthorityID          string   `json:"authority_id,omitempty"`
	MandateRef           string   `json:"mandate_ref,omitempty"`
	DelegationRef        string   `json:"delegation_ref,omitempty"`
	GrantRef             string   `json:"grant_ref,omitempty"`
	WorkloadID           string   `json:"workload_id,omitempty"`
	ExecutionID          string   `json:"execution_id,omitempty"`
	InvocationID         string   `json:"invocation_id,omitempty"`
	DecisionID           string   `json:"decision_id,omitempty"`
	PolicyRevisionID     string   `json:"policy_revision_id,omitempty"`
	AuthorizationEpoch   int64    `json:"authorization_epoch"`
	RevocationEpoch      int64    `json:"revocation_epoch"`
	Arrangement          string   `json:"arrangement,omitempty"`
	OutboxSeq            int64    `json:"outbox_seq,omitempty"`
	// ChainHead and ChainLength are set on run.steps.anchored: the anchored
	// run's head hash and step count.
	ChainHead   string `json:"chain_head,omitempty"`
	ChainLength int64  `json:"chain_length,omitempty"`
}

// AdminAuditDetails is details for source admin_audit.
type AdminAuditDetails struct {
	RowID      int64    `json:"row_id"`
	HTTPMethod string   `json:"http_method,omitempty"`
	HTTPRoute  string   `json:"http_route,omitempty"`
	StatusCode *int32   `json:"status_code,omitempty"`
	DurationMS *float64 `json:"duration_ms,omitempty"`
	IsError    bool     `json:"is_error"`
}

// storedSecurityEvent is the stored CloudEvents form, decoded leniently: the
// exporter must keep working when a later version adds a field.
type storedSecurityEvent struct {
	ID              string `json:"id"`
	Type            string `json:"type"`
	Subject         string `json:"subject"`
	Time            string `json:"time"`
	OCSFClassUID    int    `json:"ocsfclassuid"`
	OCSFCategoryUID int    `json:"ocsfcategoryuid"`
	Data            struct {
		TenantProjectID      int64    `json:"tenant_project_id"`
		Action               string   `json:"action"`
		Effect               string   `json:"effect"`
		Reasons              []string `json:"reasons"`
		PrincipalRef         string   `json:"principal_ref"`
		ActorChain           []string `json:"actor_chain"`
		ActorFidelity        string   `json:"actor_fidelity"`
		CallingApplicationID string   `json:"calling_application_id"`
		AuthenticationRef    string   `json:"authentication_ref"`
		AgentBindingID       string   `json:"agent_binding_id"`
		AuthorityID          string   `json:"authority_id"`
		MandateRef           string   `json:"mandate_ref"`
		DelegationRef        string   `json:"delegation_ref"`
		GrantRef             string   `json:"grant_ref"`
		WorkloadID           string   `json:"workload_id"`
		ExecutionID          string   `json:"execution_id"`
		InvocationID         string   `json:"invocation_id"`
		DecisionID           string   `json:"decision_id"`
		ResourceType         string   `json:"resource_type"`
		ResourceID           string   `json:"resource_id"`
		PolicyRevisionID     string   `json:"policy_revision_id"`
		AuthorizationEpoch   int64    `json:"authorization_epoch"`
		RevocationEpoch      int64    `json:"revocation_epoch"`
		Arrangement          string   `json:"arrangement"`
		CorrelationID        string   `json:"correlation_id"`
		ChainHead            string   `json:"chain_head"`
		ChainLength          int64    `json:"chain_length"`
	} `json:"data"`
}

// FromSecurityRecord maps one security audit record to the envelope.
func FromSecurityRecord(record securityaudit.Record) (Event, error) {
	var stored storedSecurityEvent
	if err := json.Unmarshal(record.Event, &stored); err != nil {
		return Event{}, fmt.Errorf("security audit record seq %d: decode stored event: %w", record.Seq, err)
	}
	if stored.ID == "" || stored.Time == "" {
		return Event{}, fmt.Errorf("security audit record seq %d: stored event has no id or time", record.Seq)
	}
	d := stored.Data
	project := d.TenantProjectID

	actor := Actor{Type: "unknown", ID: "unknown", Chain: d.ActorChain, Fidelity: d.ActorFidelity}
	switch {
	case d.PrincipalRef != "":
		actor.Type, actor.ID = "principal", d.PrincipalRef
	case d.AgentBindingID != "":
		actor.Type, actor.ID = "agent", d.AgentBindingID
	}
	outcome := OutcomeSuccess
	switch d.Effect {
	case "allow":
		outcome = OutcomeAllow
	case "deny":
		outcome = OutcomeDeny
	}
	var target *Target
	if d.ResourceType != "" || d.ResourceID != "" {
		target = &Target{Type: d.ResourceType, ID: d.ResourceID}
	}
	var correlation *Correlation
	if d.CorrelationID != "" {
		correlation = &Correlation{CorrelationID: d.CorrelationID}
	}
	reasons := d.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	partition := record.PartitionKey
	return Event{
		Schema:    SchemaVersion,
		EventID:   stored.ID,
		Time:      stored.Time,
		Source:    SourceSecurityAudit,
		EventType: stored.Type,
		OCSF:      &OCSF{ClassUID: stored.OCSFClassUID, CategoryUID: stored.OCSFCategoryUID},
		Tenant:    Tenant{ProjectID: &project},
		Actor:     actor,
		Action:    d.Action,
		Target:    target,
		Outcome:   outcome,

		Correlation: correlation,
		Chain: &Chain{
			Partition: partition,
			Sequence:  record.PartitionSeq,
			PrevHash:  hex.EncodeToString(record.PrevHash[:]),
			Hash:      hex.EncodeToString(record.Hash[:]),
		},
		Details: SecurityAuditDetails{
			Reasons:              reasons,
			CallingApplicationID: d.CallingApplicationID,
			AuthenticationRef:    d.AuthenticationRef,
			AgentBindingID:       d.AgentBindingID,
			AuthorityID:          d.AuthorityID,
			MandateRef:           d.MandateRef,
			DelegationRef:        d.DelegationRef,
			GrantRef:             d.GrantRef,
			WorkloadID:           d.WorkloadID,
			ExecutionID:          d.ExecutionID,
			InvocationID:         d.InvocationID,
			DecisionID:           d.DecisionID,
			PolicyRevisionID:     d.PolicyRevisionID,
			AuthorizationEpoch:   d.AuthorizationEpoch,
			RevocationEpoch:      d.RevocationEpoch,
			Arrangement:          d.Arrangement,
			OutboxSeq:            record.Seq,
			ChainHead:            d.ChainHead,
			ChainLength:          d.ChainLength,
		},
		ref: string(SourceSecurityAudit) + ":" + strconv.FormatInt(record.Seq, 10),
	}, nil
}

// AuditRow is one centry.audit_events row, the columns the exporter reads.
type AuditRow struct {
	ID           int64
	Timestamp    time.Time
	UserID       *int64
	UserEmail    string
	ProjectID    *int64
	EventType    string
	Action       string
	HTTPMethod   string
	HTTPRoute    string
	StatusCode   *int32
	DurationMS   *float64
	IsError      bool
	EntityType   string
	EntityID     *int64
	EntityName   string
	TraceID      string
	SpanID       string
	ParentSpanID string
	// The chain position and links (shared migration 0206, from
	// elitea_runtime.audit_events_chain). Nil on a row written before the
	// chain existed.
	PartitionKey *string
	PartitionSeq *int64
	PrevHash     []byte
	Hash         []byte
}

// auditNamespace derives event ids of audit_events rows. The id is a pure
// function of the row id, so a re-export carries the same event_id.
var auditNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:elitea:audit:centry.audit_events"))

// AuditEventID is the event_id of an audit_events row.
func AuditEventID(rowID int64) string {
	return uuid.NewSHA1(auditNamespace, []byte(strconv.FormatInt(rowID, 10))).String()
}

// FromAuditRow maps one audit_events row to the envelope.
func FromAuditRow(row AuditRow) Event {
	actor := Actor{Type: "unknown", ID: "unknown", Email: row.UserEmail}
	switch {
	case row.UserID != nil:
		actor.Type, actor.ID = "user", strconv.FormatInt(*row.UserID, 10)
	case row.EventType == "schedule" || row.EventType == "admin_task":
		actor.Type, actor.ID = "system", row.EventType
	}
	outcome := OutcomeSuccess
	if row.IsError {
		outcome = OutcomeFailure
	}
	action := strings.TrimSpace(row.Action)
	if action == "" {
		action = row.EventType
	}
	var ocsf *OCSF
	if row.EventType == "api" {
		ocsf = &OCSF{ClassUID: securityaudit.OCSFClassAPIActivity, CategoryUID: securityaudit.OCSFCategoryApplicationActivity}
	}
	var target *Target
	if row.EntityType != "" || row.EntityID != nil || row.EntityName != "" {
		target = &Target{Type: row.EntityType, Name: row.EntityName}
		if row.EntityID != nil {
			target.ID = strconv.FormatInt(*row.EntityID, 10)
		}
	}
	var correlation *Correlation
	if row.TraceID != "" || row.SpanID != "" || row.ParentSpanID != "" {
		correlation = &Correlation{TraceID: row.TraceID, SpanID: row.SpanID, ParentSpanID: row.ParentSpanID}
	}
	// A row from before the chain, or one whose links are malformed, carries
	// no chain object rather than one that cannot verify.
	var chain *Chain
	if row.PartitionKey != nil && row.PartitionSeq != nil && *row.PartitionSeq >= 1 &&
		len(row.PrevHash) == len(securityaudit.Hash{}) && len(row.Hash) == len(securityaudit.Hash{}) {
		chain = &Chain{
			Partition: *row.PartitionKey,
			Sequence:  *row.PartitionSeq,
			PrevHash:  hex.EncodeToString(row.PrevHash),
			Hash:      hex.EncodeToString(row.Hash),
		}
	}
	return Event{
		Schema:      SchemaVersion,
		EventID:     AuditEventID(row.ID),
		Time:        row.Timestamp.UTC().Format(time.RFC3339Nano),
		Source:      SourceAdminAudit,
		EventType:   row.EventType,
		OCSF:        ocsf,
		Tenant:      Tenant{ProjectID: row.ProjectID},
		Actor:       actor,
		Action:      action,
		Target:      target,
		Outcome:     outcome,
		Correlation: correlation,
		Chain:       chain,
		Details: AdminAuditDetails{
			RowID: row.ID, HTTPMethod: row.HTTPMethod, HTTPRoute: row.HTTPRoute,
			StatusCode: row.StatusCode, DurationMS: row.DurationMS, IsError: row.IsError,
		},
		ref: string(SourceAdminAudit) + ":" + strconv.FormatInt(row.ID, 10),
	}
}
