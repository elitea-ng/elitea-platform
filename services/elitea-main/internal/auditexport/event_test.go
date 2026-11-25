package auditexport

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const schemaPath = "../../../../libs/jsonschema/audit/v1/event.schema.json"

func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource("https://schemas.elitea.ai/audit/v1/event.schema.json", doc); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("https://schemas.elitea.ai/audit/v1/event.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func validate(t *testing.T, schema *jsonschema.Schema, v any) error {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return schema.Validate(doc)
}

// securityRecord builds a Record the way the appender stores it.
func securityRecord(t *testing.T, typ securityaudit.EventType, mutate func(*securityaudit.SecurityEvent)) securityaudit.Record {
	t.Helper()
	event := securityaudit.SecurityEvent{
		ID: "0192f0c1-7b6e-7c3a-8f00-000000000001", Type: typ, Source: "elitea-main/test",
		Time:            time.Date(2026, 10, 28, 9, 30, 0, 123000000, time.UTC),
		TenantProjectID: 42, Action: "credential.use", Effect: securityaudit.EffectAllow,
		Reasons:      []string{"grant.matched"},
		PrincipalRef: "user:7", ActorChain: []string{"agent:a1"}, ActorFidelity: securityaudit.FidelityNativeAct,
		AgentBindingID: "binding-1", GrantRef: "grant-9", ExecutionID: "exec-3",
		ResourceType: "credential", ResourceID: "cred-5", PolicyRevisionID: "rev-2",
		AuthorizationEpoch: 4, RevocationEpoch: 1, Arrangement: securityaudit.ArrangementB, CorrelationID: "corr-1",
	}
	if mutate != nil {
		mutate(&event)
	}
	canonical, err := event.Canonical(5)
	if err != nil {
		t.Fatal(err)
	}
	prev := securityaudit.Hash{1}
	hash := securityaudit.ChainHash(prev, canonical)
	return securityaudit.Record{
		Seq: 105, PartitionKey: event.PartitionKey(), PartitionSeq: 5, EventID: event.ID,
		EventType: typ, Event: canonical, PrevHash: prev, Hash: hash, CreatedAt: event.Time,
	}
}

func TestSecurityMapperOutputConformsToSchema(t *testing.T) {
	schema := compileSchema(t)
	for _, typ := range []securityaudit.EventType{
		securityaudit.TypeDecision, securityaudit.TypeCredentialRedeem, securityaudit.TypeGrantChanged,
		securityaudit.TypeBindingChanged, securityaudit.TypeMandateChanged, securityaudit.TypeDelegationChanged,
		securityaudit.TypeEpochBumped, securityaudit.TypeExternalIdentityChanged, securityaudit.TypeExternalIdentityDrift,
		securityaudit.TypeAgentSession, securityaudit.TypeProbe,
	} {
		t.Run(string(typ), func(t *testing.T) {
			event, err := FromSecurityRecord(securityRecord(t, typ, nil))
			if err != nil {
				t.Fatal(err)
			}
			if err := validate(t, schema, event); err != nil {
				t.Fatalf("security audit event does not conform to elitea.audit.v1: %v", err)
			}
		})
	}
}

func TestSecurityMapperMinimalAndPlatformEventsConform(t *testing.T) {
	schema := compileSchema(t)
	record := securityRecord(t, securityaudit.TypeEpochBumped, func(e *securityaudit.SecurityEvent) {
		e.TenantProjectID = 0
		e.Effect, e.Reasons, e.ActorChain, e.ActorFidelity = "", nil, nil, ""
		e.PrincipalRef, e.AgentBindingID, e.ResourceType, e.ResourceID, e.CorrelationID = "", "", "", "", ""
		e.GrantRef, e.ExecutionID, e.PolicyRevisionID, e.Arrangement = "", "", "", ""
	})
	event, err := FromSecurityRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := validate(t, schema, event); err != nil {
		t.Fatalf("minimal platform event: %v", err)
	}
	if event.Actor.Type != "unknown" || event.Outcome != OutcomeSuccess || *event.Tenant.ProjectID != 0 {
		t.Fatalf("unexpected defaults: %+v", event)
	}
}

func TestSecurityMapperFields(t *testing.T) {
	record := securityRecord(t, securityaudit.TypeDecision, func(e *securityaudit.SecurityEvent) { e.Effect = securityaudit.EffectDeny })
	event, err := FromSecurityRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if event.EventID != record.EventID || event.Source != SourceSecurityAudit || event.Outcome != OutcomeDeny {
		t.Fatalf("identity fields: %+v", event)
	}
	if event.OCSF == nil || event.OCSF.ClassUID != securityaudit.OCSFClassAuthorizeSession || event.OCSF.CategoryUID != 3 {
		t.Fatalf("ocsf = %+v", event.OCSF)
	}
	if event.Chain == nil || event.Chain.Sequence != 5 || event.Chain.Partition != "42" || len(event.Chain.Hash) != 64 {
		t.Fatalf("chain = %+v", event.Chain)
	}
	if event.Actor.Type != "principal" || event.Actor.ID != "user:7" || event.Actor.Fidelity != "native_act" || len(event.Actor.Chain) != 1 {
		t.Fatalf("actor = %+v", event.Actor)
	}
	if event.Target == nil || event.Target.Type != "credential" || event.Target.ID != "cred-5" {
		t.Fatalf("target = %+v", event.Target)
	}
	if event.Ref() != "security_audit:105" {
		t.Fatalf("ref = %q", event.Ref())
	}
}

func TestSecurityMapperRefusesUndecodableRecord(t *testing.T) {
	if _, err := FromSecurityRecord(securityaudit.Record{Seq: 1, Event: []byte(`not json`)}); err == nil {
		t.Fatal("garbage record mapped")
	}
	if _, err := FromSecurityRecord(securityaudit.Record{Seq: 1, Event: []byte(`{}`)}); err == nil {
		t.Fatal("record without id mapped")
	}
}

func ptr[T any](v T) *T { return &v }

func TestAuditRowMapperOutputConformsToSchema(t *testing.T) {
	schema := compileSchema(t)
	at := time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC)
	rows := map[string]AuditRow{
		"api request": {
			ID: 1001, Timestamp: at, UserID: ptr(int64(7)), UserEmail: "a@example.com", ProjectID: ptr(int64(3)),
			EventType: "api", Action: "POST /api/v2/applications", HTTPMethod: "POST", HTTPRoute: "/api/v2/applications",
			StatusCode: ptr(int32(201)), DurationMS: ptr(12.5), EntityType: "application", EntityID: ptr(int64(9)), EntityName: "bot",
			TraceID: "0af7651916cd43dd8448eb211c80319c", SpanID: "b7ad6b7169203331",
		},
		"failed request": {ID: 1002, Timestamp: at, UserID: ptr(int64(7)), EventType: "api", Action: "GET /x", IsError: true, StatusCode: ptr(int32(500))},
		"scheduled task": {ID: 1003, Timestamp: at, EventType: "schedule", Action: "nightly"},
		"minimal":        {ID: 1004, Timestamp: at, EventType: "tool", Action: ""},
		"chained": {
			ID: 1005, Timestamp: at, EventType: "api", Action: "GET /x",
			PartitionKey: ptr("0"), PartitionSeq: ptr(int64(12)), PrevHash: make([]byte, 32), Hash: bytes.Repeat([]byte{0xab}, 32),
		},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			event := FromAuditRow(row)
			if err := validate(t, schema, event); err != nil {
				t.Fatalf("audit_events row does not conform to elitea.audit.v1: %v", err)
			}
		})
	}
}

func TestAuditRowMapperFields(t *testing.T) {
	at := time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC)
	event := FromAuditRow(AuditRow{ID: 7, Timestamp: at, UserID: ptr(int64(7)), EventType: "api", Action: " GET /x ", IsError: true})
	if event.Outcome != OutcomeFailure || event.Actor.Type != "user" || event.Actor.ID != "7" || event.Action != "GET /x" {
		t.Fatalf("event = %+v", event)
	}
	if event.OCSF == nil || event.OCSF.ClassUID != securityaudit.OCSFClassAPIActivity {
		t.Fatalf("api rows are OCSF API Activity: %+v", event.OCSF)
	}
	if event.Chain != nil {
		t.Fatal("a row from before the chain carries no chain object")
	}
	chained := FromAuditRow(AuditRow{ID: 8, Timestamp: at, EventType: "api", Action: "GET /x",
		PartitionKey: ptr("0"), PartitionSeq: ptr(int64(3)), PrevHash: make([]byte, 32), Hash: bytes.Repeat([]byte{1}, 32)})
	if c := chained.Chain; c == nil || c.Partition != "0" || c.Sequence != 3 ||
		c.PrevHash != strings.Repeat("00", 32) || c.Hash != strings.Repeat("01", 32) {
		t.Fatalf("chained row: chain = %+v", chained.Chain)
	}
	if FromAuditRow(AuditRow{ID: 9, Timestamp: at, EventType: "api", Action: "GET /x",
		PartitionKey: ptr("0"), PartitionSeq: ptr(int64(3)), PrevHash: []byte{1}, Hash: []byte{2}}).Chain != nil {
		t.Fatal("malformed links must not be exported as a chain")
	}
	if event.EventID != AuditEventID(7) || AuditEventID(7) == AuditEventID(8) {
		t.Fatal("event id must be a stable function of the row id")
	}
	if FromAuditRow(AuditRow{ID: 7, Timestamp: at, EventType: "schedule", Action: "n"}).Actor.Type != "system" {
		t.Fatal("scheduler rows are system actors")
	}
}

func TestSchemaRejectsMalformedEvents(t *testing.T) {
	schema := compileSchema(t)
	good := FromAuditRow(AuditRow{ID: 1, Timestamp: time.Now(), UserID: ptr(int64(1)), EventType: "api", Action: "GET /x"})
	raw, _ := json.Marshal(good)
	var base map[string]any
	_ = json.Unmarshal(raw, &base)

	mutations := map[string]func(map[string]any){
		"wrong schema version": func(m map[string]any) { m["schema"] = "elitea.audit.v2" },
		"unknown source":       func(m map[string]any) { m["source"] = "mystery" },
		"unknown field":        func(m map[string]any) { m["extra"] = 1 },
		"missing outcome":      func(m map[string]any) { delete(m, "outcome") },
		"bad time":             func(m map[string]any) { m["time"] = "yesterday" },
		"admin details extra":  func(m map[string]any) { m["details"].(map[string]any)["surprise"] = true },
		"admin without row id": func(m map[string]any) { delete(m["details"].(map[string]any), "row_id") },
		// The reason text is never exported (ADR-0059 R1.4b): the schema has
		// no field for it.
		"admin reason text": func(m map[string]any) { m["details"].(map[string]any)["reason"] = "INC-42" },
		"admin unknown credential kind": func(m map[string]any) {
			m["details"].(map[string]any)["credential_kind"] = "password"
		},
		"security without chain": func(m map[string]any) {
			m["source"] = "security_audit"
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			mutate(m)
			if err := validate(t, schema, m); err == nil {
				t.Fatal("schema accepted a malformed event")
			}
		})
	}
	if err := validate(t, schema, base); err != nil {
		t.Fatalf("baseline event rejected: %v", err)
	}
}

// goldenDir holds the elitea.audit.v1 envelopes of admin_audit rows as a
// receiver sees them, byte for byte: the field order is part of the contract
// (append, never reorder). Regenerate with ELITEA_UPDATE_GOLDEN=1.
const goldenDir = "../../../../libs/jsonschema/audit/v1/testdata"

func adminAuditGoldenRows() map[string]AuditRow {
	at := time.Date(2026, 11, 25, 9, 30, 0, 0, time.UTC)
	chain := func(row AuditRow) AuditRow {
		row.PartitionKey, row.PartitionSeq = ptr("0"), ptr(int64(row.ID))
		row.PrevHash, row.Hash = bytes.Repeat([]byte{0x11}, 32), bytes.Repeat([]byte{0x22}, 32)
		return row
	}
	return map[string]AuditRow{
		// A row from before shared 0268: no role-use field at all.
		"admin_audit_before_role_use": chain(AuditRow{
			ID: 41, Timestamp: at, UserID: ptr(int64(7)), UserEmail: "a@example.com", EventType: "api",
			Action: "PUT /api/v2/admin/roles", HTTPMethod: "PUT", HTTPRoute: "/api/v2/admin/roles",
			StatusCode: ptr(int32(200)), DurationMS: ptr(3.5),
		}),
		// An administration-mode write under an elevation, every field set.
		"admin_audit_role_use": chain(AuditRow{
			ID: 42, Timestamp: at, UserID: ptr(int64(7)), UserEmail: "a@example.com", ProjectID: ptr(int64(3)),
			EventType: "api", Action: "PUT /api/v2/admin/projects/{project_id}/members",
			HTTPMethod: "PUT", HTTPRoute: "/api/v2/admin/projects/{project_id}/members",
			StatusCode: ptr(int32(200)), DurationMS: ptr(12.25),
			ActingRoles: ptr([]string{"admin", "llm_manager"}), ElevationID: "0192f0c1-7b6e-7c3a-8f00-000000000001",
			CredentialKind: "session", CentralOverride: ptr(true), ReasonCode: "incident",
			ReasonMissing: ptr(false), ElevationFlagged: ptr(true),
		}),
		// A credential that acts with no administration role: {} is a value,
		// not an absence.
		"admin_audit_no_roles": chain(AuditRow{
			ID: 43, Timestamp: at, EventType: "api", Action: "POST /api/v2/admin/tasks",
			HTTPMethod: "POST", HTTPRoute: "/api/v2/admin/tasks", StatusCode: ptr(int32(202)),
			ActingRoles: ptr([]string{}), CredentialKind: "system", CentralOverride: ptr(false),
			ElevationFlagged: ptr(false),
		}),
	}
}

func TestAdminAuditGoldenEnvelopes(t *testing.T) {
	schema := compileSchema(t)
	for name, row := range adminAuditGoldenRows() {
		t.Run(name, func(t *testing.T) {
			got, err := json.MarshalIndent(FromAuditRow(row), "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := goldenDir + "/" + name + ".golden.json"
			if os.Getenv("ELITEA_UPDATE_GOLDEN") == "1" {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (regenerate with ELITEA_UPDATE_GOLDEN=1): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("envelope differs from %s:\n%s", path, got)
			}
			var doc any
			if err := json.Unmarshal(want, &doc); err != nil {
				t.Fatal(err)
			}
			if err := validate(t, schema, doc); err != nil {
				t.Fatalf("golden %s does not conform to elitea.audit.v1: %v", name, err)
			}
			if bytes.Contains(want, []byte(`"reason"`)) {
				t.Fatal("the reason text must never be exported")
			}
		})
	}
}

// A consumer written against the envelope before the role-use fields keeps
// decoding it: the fields are additions inside details, and the mapper of
// the old details shape ignores them.
func TestAdminAuditRoleUseFieldsAreToleratedByAnOlderConsumer(t *testing.T) {
	raw, err := os.ReadFile(goldenDir + "/admin_audit_role_use.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var older struct {
		Schema  string `json:"schema"`
		Details struct {
			RowID   int64 `json:"row_id"`
			IsError bool  `json:"is_error"`
		} `json:"details"`
	}
	if err := json.Unmarshal(raw, &older); err != nil || older.Schema != SchemaVersion || older.Details.RowID != 42 {
		t.Fatalf("an older consumer cannot read the envelope: %+v, %v", older, err)
	}
	// And the order: the new fields follow is_error, the old fields keep
	// their places.
	text := string(raw)
	order := []string{`"row_id"`, `"http_method"`, `"http_route"`, `"status_code"`, `"duration_ms"`, `"is_error"`,
		`"acting_roles"`, `"elevation_id"`, `"credential_kind"`, `"central_override"`, `"reason_code"`,
		`"reason_missing"`, `"elevation_flagged"`}
	last := -1
	for _, key := range order {
		at := strings.Index(text, key)
		if at <= last {
			t.Fatalf("%s is out of order in the details", key)
		}
		last = at
	}
}
