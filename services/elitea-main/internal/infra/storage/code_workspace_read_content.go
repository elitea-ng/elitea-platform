package storage

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"math"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

type CodeWorkspaceReadVerifier interface {
	VerifyCodeWorkspaceRead(*tls.ConnectionState, *runtimev1.SignedSandboxJobGrantV1, []byte, []byte, time.Time) (CodeWorkspaceReadAuthority, error)
}
type CodeWorkspaceReadServer struct {
	verifier   CodeWorkspaceReadVerifier
	visits     CodeWorkspaceReadConsumer
	workspaces *CodeWorkspaceService
}

func NewCodeWorkspaceReadServer(verifier CodeWorkspaceReadVerifier, visits CodeWorkspaceReadConsumer, workspaces *CodeWorkspaceService) (*CodeWorkspaceReadServer, error) {
	if verifier == nil || visits == nil || workspaces == nil {
		return nil, ErrCodeWorkspaceInvalid
	}
	return &CodeWorkspaceReadServer{verifier, visits, workspaces}, nil
}
func (s *ContentServer) WithCodeWorkspaceReads(reads *CodeWorkspaceReadServer) *ContentServer {
	if s != nil {
		s.codeWorkspaceReads = reads
	}
	return s
}

type codeWorkspaceReadRequest struct {
	Schema   string  `json:"schema"`
	Grant    string  `json:"grant_base64url"`
	Prepared string  `json:"prepared_job_json_base64url"`
	Intent   *string `json:"code_execution_intent_json_base64url"`
}

func parseCodeWorkspaceReadRequest(body []byte) (*runtimev1.SignedSandboxJobGrantV1, []byte, []byte, error) {
	if len(body) == 0 || len(body) > 2<<20 || codePreparedTokens(body) != nil {
		return nil, nil, nil, ErrContentUnauthorized
	}
	var object map[string]json.RawMessage
	if codePreparedDecode(body, &object) != nil || len(object) != 4 {
		return nil, nil, nil, ErrContentUnauthorized
	}
	for _, key := range []string{"schema", "grant_base64url", "prepared_job_json_base64url", "code_execution_intent_json_base64url"} {
		if _, ok := object[key]; !ok {
			return nil, nil, nil, ErrContentUnauthorized
		}
	}
	var request codeWorkspaceReadRequest
	if codePreparedDecode(body, &request) != nil || request.Schema != "elitea.sandbox.workspace-content-read.v1" {
		return nil, nil, nil, ErrContentUnauthorized
	}
	canonical, err := sandboxJSON(request)
	if err != nil || !bytes.Equal(canonical, body) {
		return nil, nil, nil, ErrContentUnauthorized
	}
	decode := func(value string, limit int) ([]byte, error) {
		if value == "" || len(value) > base64.RawURLEncoding.EncodedLen(limit) {
			return nil, ErrContentUnauthorized
		}
		data, e := base64.RawURLEncoding.DecodeString(value)
		if e != nil || len(data) > limit || base64.RawURLEncoding.EncodeToString(data) != value {
			return nil, ErrContentUnauthorized
		}
		return data, nil
	}
	grantBytes, err := decode(request.Grant, 8192)
	if err != nil {
		return nil, nil, nil, err
	}
	var grant runtimev1.SignedSandboxJobGrantV1
	if proto.Unmarshal(grantBytes, &grant) != nil {
		return nil, nil, nil, ErrContentUnauthorized
	}
	prepared, err := decode(request.Prepared, 1<<20)
	if err != nil {
		return nil, nil, nil, err
	}
	var intent []byte
	if request.Intent != nil {
		intent, err = decode(*request.Intent, 16384)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	return &grant, prepared, intent, nil
}

// ReadSandboxWorkspaceManifest serves a workspace manifest to the sandbox
// supervisor holding a signed workspace-read proof for it.
func (s *ContentServer) ReadSandboxWorkspaceManifest(ctx context.Context, request *runtimev1.ReadSandboxWorkspaceManifestRequestV1) (*runtimev1.ReadSandboxWorkspaceManifestResponseV1, error) {
	payload, err := s.readCodeWorkspaceContent(ctx, request.GetManifestSha256(), "", request.GetRequestJson(), true)
	if err != nil {
		return nil, err
	}
	return &runtimev1.ReadSandboxWorkspaceManifestResponseV1{ManifestJson: payload}, nil
}

// ReadSandboxWorkspaceFile serves one file of that manifest.
func (s *ContentServer) ReadSandboxWorkspaceFile(ctx context.Context, request *runtimev1.ReadSandboxWorkspaceFileRequestV1) (*runtimev1.ReadSandboxWorkspaceFileResponseV1, error) {
	payload, err := s.readCodeWorkspaceContent(ctx, request.GetManifestSha256(), request.GetContentSha256(), request.GetRequestJson(), false)
	if err != nil {
		return nil, err
	}
	return &runtimev1.ReadSandboxWorkspaceFileResponseV1{Content: payload}, nil
}

func (s *ContentServer) readCodeWorkspaceContent(ctx context.Context, root, file string, body []byte, metadata bool) ([]byte, error) {
	if s.codeWorkspaceReads == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	release, err := acquireContentSlot(s.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	connection, ok := contentPeer(ctx)
	if !ok || len(body) == 0 || len(body) > maxCodeWorkspaceRequestBytes {
		return nil, contentStatus(codes.Unauthenticated)
	}
	if !workspaceHex(root, 64) || !metadata && !workspaceHex(file, 64) {
		return nil, contentStatus(codes.InvalidArgument)
	}
	grant, prepared, intent, err := parseCodeWorkspaceReadRequest(body)
	if err != nil {
		return nil, codeWorkspaceStatus(err)
	}
	authority, err := s.codeWorkspaceReads.verifier.VerifyCodeWorkspaceRead(connection, grant, intent, prepared, time.Now())
	if err != nil {
		return nil, codeWorkspaceStatus(ErrContentUnauthorized)
	}
	payload, err := s.codeWorkspaceReads.read(ctx, authority, prepared, root, file, metadata)
	if err != nil {
		return nil, codeWorkspaceStatus(err)
	}
	return payload, nil
}
func (s *CodeWorkspaceReadServer) read(ctx context.Context, authority CodeWorkspaceReadAuthority, prepared []byte, root, file string, metadata bool) ([]byte, error) {
	var scope SandboxBundleScope
	var selected CodePreparedRequest
	validate := func(ctx context.Context, tx CodeTransaction, visit OriginalCodeVisit, job CodePreparedRequest) error {
		if job.Workspace == nil || job.Workspace.ManifestSHA256 != root || visit.ResourceProjectID <= 0 || visit.ResourceProjectID > math.MaxInt32 {
			return ErrContentUnauthorized
		}
		if err := s.workspaces.verifyWorkspaceVisit(ctx, tx, visit, job); err != nil {
			return err
		}
		var err error
		scope, err = NewSandboxBundleScope(visit.TenantID, int32(visit.ResourceProjectID))
		if err == nil {
			selected = job
		}
		return err
	}
	if err := s.visits.WithCodeWorkspaceRead(ctx, authority, prepared, validate); err != nil {
		return nil, err
	}
	// Binary/object reads are outside the short current-writer transaction.
	manifest, err := s.workspaces.store.OpenCodeWorkspace(ctx, scope, root, s.workspaces.policy)
	if err != nil {
		return nil, err
	}
	expected, _ := sandboxJSON(manifest.Selection())
	actual, _ := sandboxJSON(selected.Workspace.Selection)
	policy, err := manifest.Policy().SHA256()
	if err != nil || !bytes.Equal(expected, actual) || policy != selected.Workspace.PolicySHA256 {
		return nil, ErrContentUnauthorized
	}
	var payload []byte
	if metadata {
		payload = manifest.Bytes()
	} else {
		entry, err := manifest.file("data/" + file)
		if err != nil {
			return nil, ErrContentUnauthorized
		}
		var buffer bytes.Buffer
		if err = s.workspaces.store.FetchFile(ctx, scope, manifest, entry.Name, &buffer); err != nil {
			return nil, err
		}
		payload = buffer.Bytes()
	}
	// A cancellation, takeover, or expired proof during storage IO cannot publish content.
	if err = s.visits.WithCodeWorkspaceRead(ctx, authority, prepared, validate); err != nil {
		return nil, err
	}
	return payload, nil
}
