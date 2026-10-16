package api

import (
	"errors"
	"testing"

	dbrepos "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// Review round 2: attachments land only in a SYSTEM bucket. A person-made
// bucket that holds the attachment bucket's name (created before the names
// were reserved, or by hand) is refused, never written to.
func TestAttachmentsLandOnlyInASystemBucket(t *testing.T) {
	t.Parallel()
	id, err := systemAttachmentBucket(dbrepos.BucketRow{ID: 4, BucketType: "system"})
	if err != nil || id != 4 {
		t.Fatalf("system bucket = %d, %v", id, err)
	}
	for _, kind := range []string{"local", "", "s3"} {
		if _, err := systemAttachmentBucket(dbrepos.BucketRow{ID: 5, BucketType: kind}); !errors.Is(err, errAttachmentBucketNotSystem) {
			t.Fatalf("%q bucket was accepted: %v", kind, err)
		}
	}
}
