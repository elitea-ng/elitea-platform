package s3

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

func TestArtifactCapabilitiesAreHonest(t *testing.T) {
	b := &Backend{}
	got := b.Capabilities()
	want := struct{ Presign, NativeMultipart, ServerSideCopy bool }{true, true, true}
	if !got.ChecksumSHA256 {
		t.Fatal("Capabilities().ChecksumSHA256 = false, want true")
	}
	if got.Presign != want.Presign || got.NativeMultipart != want.NativeMultipart || got.ServerSideCopy != want.ServerSideCopy {
		t.Fatalf("Capabilities() = %+v, want Presign/NativeMultipart/ServerSideCopy all true", got)
	}
}

func TestArtifactCapabilitiesWithoutPresignSendClientsToTheFacade(t *testing.T) {
	got := (&Backend{noPresign: true}).Capabilities()
	if got.Presign || got.NativeMultipart {
		t.Fatalf("Capabilities() = %+v, want Presign and NativeMultipart off: a part URL is a presigned URL", got)
	}
	if !got.ChecksumSHA256 || !got.ServerSideCopy {
		t.Fatalf("Capabilities() = %+v, want ChecksumSHA256 and ServerSideCopy kept", got)
	}
}

func TestArtifactPresignRefusedWhenPresignIsOff(t *testing.T) {
	b := &Backend{noPresign: true}
	ref, err := storage.NewObjectRef("1", "elitea-workspaces", "t/seg/a.seg")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := b.PresignGet(ctx, ref, time.Minute); !errors.Is(err, storage.ErrNotSupported) {
		t.Errorf("PresignGet error = %v, want ErrNotSupported", err)
	}
	if _, err := b.PresignPut(ctx, ref, time.Minute, storage.PutOptions{}); !errors.Is(err, storage.ErrNotSupported) {
		t.Errorf("PresignPut error = %v, want ErrNotSupported", err)
	}
	if _, err := b.PresignPart(ctx, ref, "u", 1, time.Minute); !errors.Is(err, storage.ErrNotSupported) {
		t.Errorf("PresignPart error = %v, want ErrNotSupported", err)
	}
}
