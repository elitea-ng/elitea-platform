package discovery

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/httpcache"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/branding"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/publicorigin"
)

// BrandSource is the branding resolver: one resolver for bootstrap.js,
// pack.json and the display name here, so the three never disagree.
type BrandSource interface {
	Current(ctx context.Context) branding.Snapshot
}

// Config wires a Handler.
type Config struct {
	// ServerVersion is the build version; the document publishes MajorMinor
	// of it.
	ServerVersion string
	// DeploymentKind is DeploymentKindSaaS or DeploymentKindSelfHosted;
	// anything else is replaced by DeploymentKindSelfHosted (the boot-time
	// parser in cmd/elitea-main refuses bad values, so this is defence).
	DeploymentKind string
	// PublicOrigin is publicorigin.Normalize(DEPLOYMENT_URL); empty derives
	// the origin from each request.
	PublicOrigin string
	// Brand is required.
	Brand BrandSource
	// NativeAuth and ClientPolicy may be nil (see sources.go). Pass a nil
	// INTERFACE, never a typed nil pointer.
	NativeAuth   NativeAuthSource
	ClientPolicy ClientPolicySource
	// Attachments is served as is. Its slices are copied at construction.
	Attachments AttachmentPolicy
}

// Handler serves GET and HEAD /.well-known/elitea-client.
type Handler struct {
	serverVersion  string
	deploymentKind string
	publicOrigin   string
	brand          BrandSource
	nativeAuth     NativeAuthSource
	clientPolicy   ClientPolicySource
	attachments    AttachmentPolicy
}

// NewHandler builds the handler.
func NewHandler(cfg Config) *Handler {
	kind := cfg.DeploymentKind
	if kind != DeploymentKindSaaS && kind != DeploymentKindSelfHosted {
		kind = DeploymentKindSelfHosted
	}
	return &Handler{
		serverVersion:  MajorMinor(cfg.ServerVersion),
		deploymentKind: kind,
		publicOrigin:   cfg.PublicOrigin,
		brand:          cfg.Brand,
		nativeAuth:     cfg.NativeAuth,
		clientPolicy:   cfg.ClientPolicy,
		attachments:    cloneAttachmentPolicy(cfg.Attachments),
	}
}

func cloneAttachmentPolicy(policy AttachmentPolicy) AttachmentPolicy {
	policy.AcceptedExtensions = append([]string{}, policy.AcceptedExtensions...)
	policy.InlineImageFormats = append([]string{}, policy.InlineImageFormats...)
	return policy
}

// Build assembles the document for one origin.
func (h *Handler) Build(ctx context.Context, origin string) (Document, error) {
	snap := h.brand.Current(ctx)
	doc := Document{
		ServerVersion:    h.serverVersion,
		ClientContract:   ClientContract,
		DeploymentKind:   h.deploymentKind,
		DisplayName:      snap.DisplayName(),
		BrandPackURL:     origin + branding.PackJSONPath + "?v=" + snap.PackJSONVersion(origin),
		ClientPolicy:     DefaultPublicPolicy(),
		MinClientVersion: map[string]string{},
		Attachments:      cloneAttachmentPolicy(h.attachments),
	}
	if h.nativeAuth != nil {
		na, err := h.nativeAuth.NativeAuth(ctx, origin)
		if err != nil {
			return Document{}, err
		}
		doc.NativeAuth = na
	}
	if h.clientPolicy != nil {
		policy, mins, err := h.clientPolicy.PublicPolicy(ctx)
		if err != nil {
			return Document{}, err
		}
		doc.ClientPolicy = policy
		for id, v := range mins {
			doc.MinClientVersion[id] = v
		}
	}
	return doc, nil
}

// ServeHTTP answers the document.
//
// Header matrix:
//
//	200  ETag (strong, sha256 of the body), Cache-Control: no-cache
//	304  If-None-Match matches; same ETag and Cache-Control
//	503  a source failed; Cache-Control: no-store, so no cache keeps an
//	     answer that would tell a client "no native sign-in here"
//
// The body follows database state (brand, policy, registered clients), so it
// is revalidated, never immutable. With no configured public origin the body
// is built from the request's Host and carries Vary.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	origin, fromRequest := publicorigin.Resolve(h.publicOrigin, r)
	doc, err := h.Build(r.Context(), origin)
	if err != nil {
		slog.Warn("discovery: a source failed; answering 503", "reason", err.Error())
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(`{"error":"discovery_unavailable"}`))
		}
		return
	}
	body, err := json.Marshal(doc)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	etag, _ := httpcache.StrongETag(body)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if fromRequest {
		w.Header().Set("Vary", publicorigin.VaryRequestOrigin)
	}
	if httpcache.ETagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}
