package main

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// objectStoreProbeError wraps a failed readiness probe. On GCS with no
// endpoint and no key file the credentials come from the pod's service
// account (GKE Workload Identity), and a missing
// roles/iam.workloadIdentityUser binding surfaces only as a generic
// credentials error: name the two likely causes so the log is enough (#1460).
func objectStoreProbeError(cfg storage.Config, err error) error {
	if cfg.Backend == "gcs" && cfg.GCS.Endpoint == "" && cfg.GCS.CredentialsFile == "" {
		return fmt.Errorf("object store readiness probe: %w (GCS uses Application Default Credentials: on GKE check the iam.gke.io/gcp-service-account annotation on elitea-main's service account and the roles/iam.workloadIdentityUser binding for PROJECT.svc.id.goog[<namespace>/<service account>]; helm get notes prints the commands)", err)
	}
	return fmt.Errorf("object store readiness probe: %w", err)
}
