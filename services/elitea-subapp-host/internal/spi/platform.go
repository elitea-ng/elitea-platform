package spi

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// The platform route: operations that are not toolkit tools.
//
// A toolkit tool is reachable by any user session the facade vouches for.
// Some operations are not a user's to run: project deprovisioning deletes
// everything a project owns in the application. Those are exposed here, on
// this host's one listener, under /internal/, and authorised by ONE thing:
// the verified mutual-TLS client certificate of the platform's own service
// (elitea-main), matched against the allowlist <PREFIX>PLATFORM_CLIENTS.
//
// Why this surface and not gRPC: AGENTS.md says internal service-to-service
// calls use gRPC over mTLS, but this host has no gRPC server; its only
// listener is the SPI's HTTP over mTLS (the same client CA and handshake the
// facade's calls use). The route rides that listener and the same
// certificate check, so it adds no new transport or credential; it moves to
// gRPC with the rest of the host's internal surface when that exists.
//
// What does NOT authorise it: an identity signature (the facade signs one
// for every user call, and "a signed hop with no user id" is not proof of
// anything), a header, or a body field. The /internal/ paths skip the
// signature requirement altogether and ignore the identity headers.

// PlatformOps is what a runner offers the platform route.
type PlatformOps interface {
	// DeleteProject removes everything the application holds for the
	// project, and answers what was removed. It must be idempotent.
	DeleteProject(ctx context.Context, projectID string) (map[string]any, error)
}

// DeleteProjectPath is the platform route that deletes a project's data.
const DeleteProjectPath = "/internal/v1/projects/delete"

const internalPrefix = "/internal/"

// PlatformClients parses <PREFIX>PLATFORM_CLIENTS: a comma-separated list of
// certificate identities (a subject common name or a DNS SAN).
func PlatformClients(raw string) []string {
	var clients []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			clients = append(clients, part)
		}
	}
	return clients
}

// platformCaller reports whether the request's VERIFIED client certificate
// names an allowed platform client. A cleartext request, a TLS request with
// no verified chain, an empty allowlist: all refused.
func (s *Server) platformCaller(r *http.Request) (string, bool) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return "", false
	}
	leaf := r.TLS.VerifiedChains[0][0]
	return matchClient(leaf, s.settings.PlatformClients)
}

func matchClient(leaf *x509.Certificate, allowed []string) (string, bool) {
	identities := append([]string{leaf.Subject.CommonName}, leaf.DNSNames...)
	for _, identity := range identities {
		if identity == "" {
			continue
		}
		for _, want := range allowed {
			if identity == want {
				return identity, true
			}
		}
	}
	return "", false
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.platformCaller(r)
	if !ok {
		s.logger.Warn("refusing a platform operation: no allowed client certificate", "path", r.URL.Path)
		writeJSON(w, http.StatusForbidden, TransportError(http.StatusForbidden, "Forbidden"))
		return
	}
	ops, ok := s.app.Runner.(PlatformOps)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, TransportError(http.StatusNotImplemented, "Not Implemented"))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4096+1))
	var body struct {
		ProjectID json.RawMessage `json:"project_id"`
	}
	if err != nil || len(raw) > 4096 || json.Unmarshal(raw, &body) != nil {
		writeJSON(w, http.StatusBadRequest, TransportError(http.StatusBadRequest, "Bad Request"))
		return
	}
	project := strings.Trim(strings.TrimSpace(string(body.ProjectID)), `"`)
	if id, err := strconv.ParseInt(project, 10, 32); err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, TransportError(http.StatusBadRequest, "project_id must be a positive integer"))
		return
	}
	s.logger.Info("platform operation", "op", "delete_project", "project_id", project, "caller", caller)
	result, err := ops.DeleteProject(r.Context(), project)
	if err != nil {
		s.logger.Error("platform delete_project failed", "project_id", project, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"errorCode": "500", "message": err.Error(), "details": []string{},
		})
		return
	}
	writeJSON(w, http.StatusOK, result)
}
