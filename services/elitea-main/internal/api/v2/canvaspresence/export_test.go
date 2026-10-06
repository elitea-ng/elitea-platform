package canvaspresence

import (
	"context"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// BootstrapBucket creates bucket with the settings the nats-bootstrap Job
// gives ELITEA_CANVAS_PRESENCE (deploy/helm/nats-bootstrap/files/bootstrap.sh:
// History 1, TTL 2m, file storage). The plaintext test server has no
// bootstrap of its own, and NewNATSStore only binds. The secured test in
// cmd/elitea-main runs the real script instead.
func BootstrapBucket(ctx context.Context, js jetstream.JetStream, bucket string) error {
	_, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      bucket,
		Description: "elitea-main canvas presence rosters (one entry per editor; TTL-bounded)",
		History:     1,
		TTL:         2 * time.Minute,
		Storage:     jetstream.FileStorage,
	})
	return err
}
