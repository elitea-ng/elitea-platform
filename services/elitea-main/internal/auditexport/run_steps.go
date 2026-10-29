package auditexport

import (
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runevidence"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// runStepColumns are what a run_step event carries: metadata, digests,
// object references and the chain, never content.
const runStepColumns = `s.id, s.run_id, s.seq, s.step_id::text, s.generation, s.project_id, s.application_id, s.release_id,
       s.definition_digest, s.kind, s.actor, s.node_id, s.name, s.toolkit_id, s.started_at, s.ended_at,
       s.summary::text, s.retrieval::text, s.cited_step_ids, s.cited_document_ids,
       s.inputs_digest, s.inputs_size, s.inputs_original_size, s.inputs_truncated, s.inputs_filtered, s.inputs_media_type, s.inputs_object_key,
       s.outputs_digest, s.outputs_size, s.outputs_original_size, s.outputs_truncated, s.outputs_filtered, s.outputs_media_type, s.outputs_object_key,
       s.prev_hash, s.hash`

var runStepsSpec = streamSpec{
	stream: StreamRunSteps,
	newestSQL: `SELECT export_txid, id FROM elitea_runtime.run_steps
     ORDER BY export_txid DESC, id DESC
     LIMIT 1`,
	rowsSQL: `
SELECT s.export_txid, ` + runStepColumns + `
  FROM elitea_runtime.run_steps s
 WHERE (s.export_txid, s.id) > ($1::bigint, $2::bigint)
   AND s.export_txid < pg_snapshot_xmin(pg_current_snapshot())::text::bigint
 ORDER BY s.export_txid, s.id
 LIMIT $3`,
	scan: scanRunStepRows,
}

func scanRunStepRows(rows pgx.Rows) ([]Event, []position, error) {
	defer rows.Close()
	var (
		events    []Event
		positions []position
	)
	for rows.Next() {
		var (
			txid                  int64
			r                     runevidence.Record
			kind                  string
			summary               string
			retrieval             *string
			inputsKey, outputsKey *string
		)
		if err := rows.Scan(&txid, &r.ID, &r.RunID, &r.Seq, &r.StepID, &r.Generation, &r.ProjectID, &r.ApplicationID, &r.ReleaseID,
			&r.DefinitionDigest, &kind, &r.Actor, &r.NodeID, &r.Name, &r.ToolkitID, &r.StartedAt, &r.EndedAt,
			&summary, &retrieval, &r.CitedStepIDs, &r.CitedDocumentIDs,
			&r.Inputs.Digest, &r.Inputs.Size, &r.Inputs.OriginalSize, &r.Inputs.Truncated, &r.Inputs.Filtered, &r.Inputs.MediaType, &inputsKey,
			&r.Outputs.Digest, &r.Outputs.Size, &r.Outputs.OriginalSize, &r.Outputs.Truncated, &r.Outputs.Filtered, &r.Outputs.MediaType, &outputsKey,
			&r.PrevHash, &r.Hash); err != nil {
			return nil, nil, err
		}
		r.Kind, r.Summary = runevidence.Kind(kind), []byte(summary)
		if retrieval != nil {
			r.Retrieval = []byte(*retrieval)
		}
		r.Inputs.ObjectKey, r.Outputs.ObjectKey = deref(inputsKey), deref(outputsKey)
		events = append(events, FromRunStep(r))
		positions = append(positions, position{txid: txid, id: r.ID})
	}
	return events, positions, rows.Err()
}

// RunStepDetails is details for source run_step.
type RunStepDetails struct {
	RowID            int64             `json:"row_id"`
	RunID            string            `json:"run_id"`
	StepID           string            `json:"step_id"`
	Seq              int64             `json:"seq"`
	Generation       int64             `json:"generation"`
	Kind             string            `json:"kind"`
	ApplicationID    *int64            `json:"application_id,omitempty"`
	ReleaseID        *int64            `json:"release_id,omitempty"`
	DefinitionDigest string            `json:"definition_digest,omitempty"`
	NodeID           string            `json:"node_id,omitempty"`
	Name             string            `json:"name,omitempty"`
	ToolkitID        string            `json:"toolkit_id,omitempty"`
	StartedAt        string            `json:"started_at"`
	EndedAt          string            `json:"ended_at"`
	Summary          json.RawMessage   `json:"summary"`
	Retrieval        json.RawMessage   `json:"retrieval,omitempty"`
	CitedStepIDs     []string          `json:"cited_step_ids"`
	CitedDocumentIDs []string          `json:"cited_document_ids"`
	Inputs           RunStepContentRef `json:"inputs"`
	Outputs          RunStepContentRef `json:"outputs"`
}

// RunStepContentRef is one content side by digest and reference.
type RunStepContentRef struct {
	Digest       string `json:"digest,omitempty"`
	Size         int64  `json:"size"`
	OriginalSize int64  `json:"original_size"`
	Truncated    bool   `json:"truncated"`
	Filtered     bool   `json:"filtered"`
	MediaType    string `json:"media_type,omitempty"`
	// Ref is "<bucket>/<key>" of the platform-scoped object while it is
	// stored.
	Ref string `json:"ref,omitempty"`
}

// runStepNamespace derives the event id of a step record from its run and
// step_id, so a re-export carries the same event_id.
var runStepNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:elitea:audit:elitea_runtime.run_steps"))

// RunStepEventID is the event_id of a step record.
func RunStepEventID(runID, stepID string) string {
	return uuid.NewSHA1(runStepNamespace, []byte(runID+"/"+stepID)).String()
}

// RunStepPartition is the chain partition of a run's steps.
func RunStepPartition(runID string) string { return "run:" + runID }

// FromRunStep maps one step record to the envelope.
func FromRunStep(r runevidence.Record) Event {
	actor := Actor{Type: "unknown", ID: "unknown"}
	switch {
	case strings.HasPrefix(r.Actor, "agent:"):
		actor.Type, actor.ID = "agent", strings.TrimPrefix(r.Actor, "agent:")
	case strings.HasPrefix(r.Actor, "user:"):
		actor.Type, actor.ID = "user", strings.TrimPrefix(r.Actor, "user:")
	}
	if actor.ID == "" {
		actor.ID = "unknown"
	}
	outcome := OutcomeSuccess
	if r.Kind == runevidence.KindError {
		outcome = OutcomeFailure
	}
	var target *Target
	if r.Name != "" || r.ToolkitID != "" {
		target = &Target{Type: string(r.Kind), ID: r.ToolkitID, Name: r.Name}
	}
	project := r.ProjectID
	summary := json.RawMessage(r.Summary)
	if len(summary) == 0 {
		summary = json.RawMessage("{}")
	}
	cited, documents := r.CitedStepIDs, r.CitedDocumentIDs
	if cited == nil {
		cited = []string{}
	}
	if documents == nil {
		documents = []string{}
	}
	details := RunStepDetails{
		RowID: r.ID, RunID: r.RunID, StepID: r.StepID, Seq: r.Seq, Generation: r.Generation, Kind: string(r.Kind),
		ApplicationID: r.ApplicationID, ReleaseID: r.ReleaseID, NodeID: r.NodeID, Name: r.Name, ToolkitID: r.ToolkitID,
		StartedAt: r.StartedAt.UTC().Format(time.RFC3339Nano), EndedAt: r.EndedAt.UTC().Format(time.RFC3339Nano),
		Summary: summary, CitedStepIDs: cited, CitedDocumentIDs: documents,
		Inputs: contentRef(r.Inputs), Outputs: contentRef(r.Outputs),
	}
	if r.DefinitionDigest != nil {
		details.DefinitionDigest = hex.EncodeToString(r.DefinitionDigest)
	}
	if r.Retrieval != nil {
		details.Retrieval = json.RawMessage(r.Retrieval)
	}
	return Event{
		Schema:    SchemaVersion,
		EventID:   RunStepEventID(r.RunID, r.StepID),
		Time:      r.StartedAt.UTC().Format(time.RFC3339Nano),
		Source:    SourceRunStep,
		EventType: "run_step." + string(r.Kind),
		OCSF:      &OCSF{ClassUID: securityaudit.OCSFClassAPIActivity, CategoryUID: securityaudit.OCSFCategoryApplicationActivity},
		Tenant:    Tenant{ProjectID: &project},
		Actor:     actor,
		Action:    string(r.Kind),
		Target:    target,
		Outcome:   outcome,
		Correlation: &Correlation{
			CorrelationID: r.RunID,
		},
		Chain: &Chain{
			Partition: RunStepPartition(r.RunID),
			Sequence:  r.Seq,
			PrevHash:  hex.EncodeToString(r.PrevHash),
			Hash:      hex.EncodeToString(r.Hash),
		},
		Details: details,
		ref:     string(SourceRunStep) + ":" + strconv.FormatInt(r.ID, 10),
	}
}

func contentRef(m runevidence.ContentMeta) RunStepContentRef {
	ref := RunStepContentRef{
		Size: m.Size, OriginalSize: m.OriginalSize, Truncated: m.Truncated, Filtered: m.Filtered, MediaType: m.MediaType,
	}
	if m.Digest != nil {
		ref.Digest = hex.EncodeToString(m.Digest)
	}
	if m.ObjectKey != "" {
		ref.Ref = runevidence.ContentBucket + "/" + m.ObjectKey
	}
	return ref
}
