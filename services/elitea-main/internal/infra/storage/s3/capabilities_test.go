package s3

import "testing"

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
