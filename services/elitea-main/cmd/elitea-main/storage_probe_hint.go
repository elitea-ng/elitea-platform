package main

import (
	"fmt"
	"os"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// objectStoreStartupError wraps a failed object-store startup step (the
// readiness probe, the lifecycle rule) as "<step>: <err>". On GCS with no
// endpoint, no key file and no emulator the credentials come from the pod's
// service account (GKE Workload Identity), and a missing
// roles/iam.workloadIdentityUser binding surfaces only as a generic
// credentials error: name the two likely causes so the log is enough (#1460).
func objectStoreStartupError(cfg storage.Config, step string, err error) error {
	return objectStoreStartupErrorEnv(cfg, step, err, os.LookupEnv)
}

func objectStoreStartupErrorEnv(cfg storage.Config, step string, err error, lookup func(string) (string, bool)) error {
	if cfg.Backend == "gcs" && cfg.GCS.Endpoint == "" && cfg.GCS.CredentialsFile == "" {
		keyless := true
		for _, name := range []string{"GOOGLE_APPLICATION_CREDENTIALS", "STORAGE_EMULATOR_HOST"} {
			if v, ok := lookup(name); ok && v != "" {
				keyless = false
			}
		}
		if keyless {
			return fmt.Errorf("%s: %w (GCS uses Application Default Credentials: on GKE check the iam.gke.io/gcp-service-account annotation on elitea-main's service account and the roles/iam.workloadIdentityUser binding for PROJECT.svc.id.goog[<namespace>/<service account>]; the install notes (templates/NOTES.txt) print the commands)", step, err)
		}
	}
	return fmt.Errorf("%s: %w", step, err)
}
