package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/go-chi/chi/v5"
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

func (s *ContentServer) PostCodeWorkspaceManifest(w http.ResponseWriter, r *http.Request) {
	s.postCodeWorkspaceContent(w, r, true)
}
func (s *ContentServer) PostCodeWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	s.postCodeWorkspaceContent(w, r, false)
}
func (s *ContentServer) postCodeWorkspaceContent(w http.ResponseWriter, r *http.Request, metadata bool) {
	setPrivateNoCacheHeaders(w.Header())
	if !s.acquire(w) {
		return
	}
	defer s.release()
	if s.codeWorkspaceReads == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || r.Header.Get("Content-Type") != "application/json" || r.ContentLength <= 0 || r.ContentLength > 2<<20 || len(r.TransferEncoding) != 0 {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}
	root, file := chi.URLParam(r, "manifestSHA256"), chi.URLParam(r, "contentSHA256")
	if !workspaceHex(root, 64) || !metadata && !workspaceHex(file, 64) {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, (2<<20)+1))
	if err != nil || int64(len(body)) != r.ContentLength {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	grant, prepared, intent, err := parseCodeWorkspaceReadRequest(body)
	if err != nil {
		codeWorkspaceHTTPError(w, err)
		return
	}
	authority, err := s.codeWorkspaceReads.verifier.VerifyCodeWorkspaceRead(r.TLS, grant, intent, prepared, time.Now())
	if err != nil {
		codeWorkspaceHTTPError(w, ErrContentUnauthorized)
		return
	}
	payload, err := s.codeWorkspaceReads.read(r.Context(), authority, prepared, root, file, metadata)
	if err != nil {
		codeWorkspaceHTTPError(w, err)
		return
	}
	media := "application/octet-stream"
	if metadata {
		media = "application/json"
	}
	w.Header().Set("Content-Type", media)
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.Header().Set("Content-Digest", formatSHA256Digest(sha256.Sum256(payload)))
	w.Header().Set("X-Elitea-Workspace-Root", root)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err = w.Write(payload); err != nil {
		s.logger.WarnContext(r.Context(), "Code workspace content response write failed")
	}
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
