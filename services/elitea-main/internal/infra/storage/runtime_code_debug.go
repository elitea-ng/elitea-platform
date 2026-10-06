package storage

import (
	"context"
	"encoding/json"
	"errors"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	"strconv"
	"strings"
	"unicode/utf8"
)

const CodeDebugAdmissionSchema = "elitea.runtime.code-debug-admission.v1"
const CodeDebugSnapshotSchema = "elitea.runtime.code-debug-snapshot.v1"
const CodeDebugArtifactSchema = "elitea.runtime.code-debug-artifact.v1"
const MaxCodeDebugAdmissionBytes = 3 * 1024 * 1024
const MaxCodeDebugSnapshotBytes = 3 * 1024 * 1024

// No actor, project, bucket, key, or debug flag comes from this request.
type CodeDebugAdmission struct {
	OriginalVisit     code.OriginalVisitRef `json:"original_visit"`
	Attempt           uint16                `json:"attempt"`
	SchemaVersion     string                `json:"schema_version"`
	NodeID            string                `json:"node_id"`
	GraphThreadID     string                `json:"graph_thread_id"`
	GraphStep         string                `json:"graph_step"`
	ActivationID      string                `json:"activation_id"`
	DefinitionSHA256  string                `json:"definition_sha256"`
	YAMLSHA256        string                `json:"yaml_sha256"`
	ConfigurationJSON string                `json:"configuration_json"`
	RequestSHA256     string                `json:"request_sha256"`
	SourceSHA256      string                `json:"source_sha256"`
	InputSHA256       string                `json:"input_sha256"`
	SnapshotSHA256    string                `json:"snapshot_sha256"`
	ByteLength        int64                 `json:"byte_length"`
}
type CodeDebugArtifactReference struct {
	SchemaVersion string `json:"schema_version"`
	ProjectID     int64  `json:"project_id"`
	Bucket        string `json:"bucket"`
	Name          string `json:"name"`
	MediaType     string `json:"media_type"`
	ByteLength    int64  `json:"byte_length"`
	SHA256        string `json:"sha256"`
}

// Trusted facts from a current original Code visit. Never serialized to a client.
type CodeDebugAuthorization struct {
	TenantID     string
	ProjectID    int64
	ActorID      int64
	Language     string
	ClaimAttempt uint64
	LeaseEpoch   uint64
}

// A bounded upload handle is prepared in a short original-visit transaction.
// No database lock or grant is carried across byte reads or object-store IO.
type CodeDebugUpload struct {
	Admission     CodeDebugAdmission
	Authorization CodeDebugAuthorization
	ObjectKey     string
	State         string
}

type CodeDebugArtifactRepository interface {
	StageCodeDebug(context.Context, ContentClaim, CodeDebugAdmission) error
	PrepareCodeDebugUpload(context.Context, ContentClaim, string) (CodeDebugUpload, error)
	CommitCodeDebugUpload(context.Context, ContentClaim, CodeDebugUpload, []byte) (CodeDebugArtifactReference, error)
}
type RuntimeCodeDebugArtifactService struct {
	artifacts CodeDebugArtifactRepository
	slots     chan struct{}
}

func NewRuntimeCodeDebugArtifactService(artifacts CodeDebugArtifactRepository) (*RuntimeCodeDebugArtifactService, error) {
	if artifacts == nil {
		return nil, errors.New("code debug dependencies are required")
	}
	return &RuntimeCodeDebugArtifactService{artifacts: artifacts, slots: make(chan struct{}, 4)}, nil
}
func (s *RuntimeCodeDebugArtifactService) Admit(ctx context.Context, claim ContentClaim, a CodeDebugAdmission) error {
	if ValidateCodeDebugAdmission(a) != nil {
		return ErrContentRejected
	}
	return s.artifacts.StageCodeDebug(ctx, claim, a)
}
func (s *RuntimeCodeDebugArtifactService) prepareCommit(ctx context.Context, claim ContentClaim, visitID string) (CodeDebugUpload, error) {
	if !debugHex(visitID) {
		return CodeDebugUpload{}, ErrContentRejected
	}
	return s.artifacts.PrepareCodeDebugUpload(ctx, claim, visitID)
}
func (s *RuntimeCodeDebugArtifactService) commitAuthorized(ctx context.Context, claim ContentClaim, upload CodeDebugUpload, raw []byte) (CodeDebugArtifactReference, error) {
	if ValidateCodeDebugSnapshot(raw, upload.Admission, upload.Authorization.Language) != nil {
		return CodeDebugArtifactReference{}, ErrContentRejected
	}
	return s.artifacts.CommitCodeDebugUpload(ctx, claim, upload, raw)
}
func (s *RuntimeCodeDebugArtifactService) Commit(ctx context.Context, claim ContentClaim, visitID string, raw []byte) (CodeDebugArtifactReference, error) {
	upload, err := s.prepareCommit(ctx, claim, visitID)
	if err != nil {
		return CodeDebugArtifactReference{}, err
	}
	return s.commitAuthorized(ctx, claim, upload, raw)
}

func ValidateCodeDebugAdmission(a CodeDebugAdmission) error {
	if a.OriginalVisit.Validate() != nil || a.Attempt < 1 || a.Attempt > 16 || a.SchemaVersion != CodeDebugAdmissionSchema || !codeDebugNodeID(a.NodeID) || a.GraphThreadID == "" || len(a.GraphThreadID) > 512 || strings.ContainsAny(a.GraphThreadID, "\r\n\x00") || a.ByteLength < 1 || a.ByteLength > MaxCodeDebugSnapshotBytes || len(a.ConfigurationJSON) == 0 || len(a.ConfigurationJSON) > 2*1024*1024 {
		return ErrContentRejected
	}
	step, err := strconv.ParseUint(a.GraphStep, 10, 64)
	if err != nil || strconv.FormatUint(step, 10) != a.GraphStep {
		return ErrContentRejected
	}
	for _, h := range []string{a.ActivationID, a.DefinitionSHA256, a.YAMLSHA256, a.RequestSHA256, a.SourceSHA256, a.InputSHA256, a.SnapshotSHA256} {
		if !debugHex(h) {
			return ErrContentRejected
		}
	}
	config, err := codeDebugJSONObject([]byte(a.ConfigurationJSON))
	if err != nil || config["id"] != a.NodeID || config["type"] != "code" || config["debug"] != true {
		return ErrContentRejected
	}
	return nil
}
func ValidateCodeDebugSnapshot(raw []byte, a CodeDebugAdmission, language string) error {
	if int64(len(raw)) != a.ByteLength || len(raw) > MaxCodeDebugSnapshotBytes || CodeDebugSHA256(raw) != a.SnapshotSHA256 || !utf8.Valid(raw) || uniqueCodeDebugJSON(raw) != nil {
		return ErrContentRejected
	}
	var value struct {
		SchemaVersion string          `json:"schema_version"`
		Language      string          `json:"language"`
		Source        string          `json:"source"`
		Input         json.RawMessage `json:"selected_input"`
	}
	if strictCodeDebugJSON(raw, &value) != nil || value.SchemaVersion != CodeDebugSnapshotSchema || value.Language != language || len(value.Source) > 256*1024 || len(value.Input) > 512*1024 || CodeDebugSHA256([]byte(value.Source)) != a.SourceSHA256 || CodeDebugSHA256(value.Input) != a.InputSHA256 {
		return ErrContentRejected
	}
	if _, err := codeDebugJSONObject(value.Input); err != nil {
		return ErrContentRejected
	}
	return nil
}
