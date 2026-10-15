package workspace_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/workspace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const (
	thread    = "0b9c6f4e-9a7f-4d4e-8f0e-1d2c3b4a5f60"
	cloudSHA  = "1111111111111111111111111111111111111111"
	nextSHA   = "2222222222222222222222222222222222222222"
	upstreamC = "3333333333333333333333333333333333333333"
)

type allowWorkload struct{}

func (allowWorkload) AuthorizeWorkload(context.Context, string, string) (string, error) {
	return "spiffe://elitea/worker", nil
}

type verifier struct{ err error }

func (v verifier) VerifyActive(context.Context, runtimedomain.Fence) error { return v.err }

// fakeStore is the server unit's Store as the executor needs it.
type fakeStore struct {
	mu        sync.Mutex
	epoch     uint64
	turn      bool
	segment   []byte
	corrupt   bool
	uploaded  map[string][]byte
	publishes []workspaceruntime.PublishRequest
	claims    []workspaceruntime.ClaimRef
	cloud     string
	source    workspaceruntime.PullRequestSource
}

func (f *fakeStore) check(claim workspaceruntime.ClaimRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = append(f.claims, claim)
	if !f.turn {
		return workspaceruntime.Refuse(workspaceruntime.RefusalNotWorkspaceTurn)
	}
	if claim.Epoch != f.epoch || claim.ThreadUUID != thread {
		return workspaceruntime.Refuse(workspaceruntime.RefusalStaleEpoch)
	}
	return nil
}

func digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func (f *fakeStore) FetchPlan(_ context.Context, claim workspaceruntime.ClaimRef, _ uint64) (workspaceruntime.FetchPlan, error) {
	if err := f.check(claim); err != nil {
		return workspaceruntime.FetchPlan{}, err
	}
	refs := []workspaceruntime.Ref{{Name: workspaceruntime.RefCloud, SHA: cloudSHA}}
	return workspaceruntime.FetchPlan{HeadSeq: 3, Refs: refs, Segments: []workspaceruntime.Segment{{
		Seq: 3, Kind: workspaceruntime.KindCompact, SHA256: digest(f.segment), Bytes: uint64(len(f.segment)), Refs: refs,
	}}}, nil
}

func (f *fakeStore) OpenSegment(_ context.Context, claim workspaceruntime.ClaimRef, sha string) (io.ReadCloser, error) {
	if err := f.check(claim); err != nil {
		return nil, err
	}
	if sha != digest(f.segment) {
		return nil, workspaceruntime.Refuse(workspaceruntime.RefusalSegmentMissing)
	}
	content := f.segment
	if f.corrupt {
		content = append([]byte(nil), content...)
		content[len(content)/2] ^= 0xff
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

func (f *fakeStore) PutSegment(_ context.Context, claim workspaceruntime.ClaimRef, sha string, size uint64, body io.Reader) (workspaceruntime.UploadResult, error) {
	if err := f.check(claim); err != nil {
		return workspaceruntime.UploadResult{}, err
	}
	content, err := io.ReadAll(body)
	if err != nil {
		return workspaceruntime.UploadResult{}, err
	}
	if uint64(len(content)) != size || digest(content) != sha {
		return workspaceruntime.UploadResult{}, workspaceruntime.Refuse(workspaceruntime.RefusalChecksumMismatch)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, exists := f.uploaded[sha]
	f.uploaded[sha] = content
	return workspaceruntime.UploadResult{Exists: exists}, nil
}

func (f *fakeStore) Publish(_ context.Context, claim workspaceruntime.ClaimRef, request workspaceruntime.PublishRequest) (workspaceruntime.PublishResult, error) {
	if err := f.check(claim); err != nil {
		return workspaceruntime.PublishResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.uploaded[request.SegmentSHA256]; !ok {
		return workspaceruntime.PublishResult{}, workspaceruntime.Refuse(workspaceruntime.RefusalSegmentMissing)
	}
	update := request.Refs[0]
	if update.Old == nil || *update.Old != f.cloud {
		return workspaceruntime.PublishResult{}, &workspaceruntime.Refusal{Code: workspaceruntime.RefusalRefConflict, Current: []workspaceruntime.Ref{{Name: workspaceruntime.RefCloud, SHA: f.cloud}}}
	}
	f.cloud = update.New
	f.publishes = append(f.publishes, request)
	return workspaceruntime.PublishResult{Seq: uint64(3 + len(f.publishes)), Refs: []workspaceruntime.Ref{{Name: workspaceruntime.RefCloud, SHA: f.cloud}}}, nil
}

func (f *fakeStore) PullRequestSource(_ context.Context, claim workspaceruntime.ClaimRef) (workspaceruntime.PullRequestSource, error) {
	if err := f.check(claim); err != nil {
		return workspaceruntime.PullRequestSource{}, err
	}
	if f.source.Blobs == nil {
		return workspaceruntime.PullRequestSource{}, workspaceruntime.Refuse(workspaceruntime.RefusalNoUpstream)
	}
	return f.source, nil
}

type blobs map[string][]byte

func (b blobs) ReadBlob(_ context.Context, oid string, _ int64) ([]byte, error) {
	content, ok := b[oid]
	if !ok {
		return nil, errors.New("missing")
	}
	return content, nil
}

func (b blobs) add(content string) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(content), content)))
	oid := hex.EncodeToString(sum[:])
	b[oid] = []byte(content)
	return oid
}

func newStore() *fakeStore {
	segment := bytes.Repeat([]byte("# v2 git bundle\n"), 10_000) // 160 KB: several chunks
	return &fakeStore{epoch: 2, turn: true, segment: segment, uploaded: map[string][]byte{}, cloud: cloudSHA}
}

// dial serves the service behind the control listener's strict codec and
// its production bounds (64 KiB requests, 80 KiB responses), so the chunk
// size is proven to fit them.
func dial(t *testing.T, store workspaceruntime.Store, claims workspace.ClaimVerifier) runtimev1.WorkspaceStoreServiceClient {
	t.Helper()
	server, err := workspace.NewServer(allowWorkload{}, claims, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := runtimegrpc.NewDirectionalStrictProtoCodec(64*1024, 80*1024)
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer(runtimegrpc.StrictServerOptions(codec)...)
	runtimev1.RegisterWorkspaceStoreServiceServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	connection, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return runtimev1.NewWorkspaceStoreServiceClient(connection)
}

func claim(epoch uint64) *runtimev1.WorkspaceClaimV1 {
	return &runtimev1.WorkspaceClaimV1{
		Identity: &runtimev1.ExecutionIdentityV1{CommandId: "command-1", ExecutionId: "execution-1", Generation: 1},
		Fence: &runtimev1.ExecutionFenceV1{
			WorkloadSessionId: "session-1", ProducerId: "producer-1", ClaimAttempt: 1, LeaseEpoch: 1,
			FenceToken: bytes.Repeat([]byte{7}, 32),
		},
		ClaimId:    "claim-1",
		ThreadUuid: thread,
		Epoch:      epoch,
	}
}

func fetchAll(t *testing.T, client runtimev1.WorkspaceStoreServiceClient, request *runtimev1.WorkspaceFetchPlanRequestV1) []*runtimev1.WorkspaceFetchFrameV1 {
	t.Helper()
	stream, err := client.WorkspaceFetchPlan(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var frames []*runtimev1.WorkspaceFetchFrameV1
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
}

func TestFetchStreamsThePlanThenTheSegmentInBoundedChunks(t *testing.T) {
	store := newStore()
	client := dial(t, store, verifier{})
	frames := fetchAll(t, client, &runtimev1.WorkspaceFetchPlanRequestV1{Claim: claim(2)})
	plan := frames[0].GetPlan()
	if plan == nil || plan.GetHeadSeq() != 3 || len(plan.GetSegments()) != 1 {
		t.Fatalf("first frame is not the plan: %v", frames[0])
	}
	var received []byte
	for _, frame := range frames[1 : len(frames)-1] {
		chunk := frame.GetSegmentChunk()
		if len(chunk) == 0 || len(chunk) > workspace.ChunkBytes {
			t.Fatalf("chunk of %d bytes", len(chunk))
		}
		received = append(received, chunk...)
	}
	end := frames[len(frames)-1].GetSegmentEnd()
	if end == nil || end.GetSha256() != digest(store.segment) || !bytes.Equal(received, store.segment) {
		t.Fatal("the segment did not arrive intact")
	}
	got := store.claims[0]
	if got.ExecutionID != "execution-1" || got.Generation != 1 || got.ClaimID != "claim-1" || got.WorkloadIdentity != "spiffe://elitea/worker" || got.Epoch != 2 {
		t.Fatalf("claim ref %+v", got)
	}
}

func TestFetchRefusesAStaleEpochAndANonWorkspaceTurn(t *testing.T) {
	store := newStore()
	client := dial(t, store, verifier{})
	frames := fetchAll(t, client, &runtimev1.WorkspaceFetchPlanRequestV1{Claim: claim(1)})
	if len(frames) != 1 || frames[0].GetRefusal().GetCode() != runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_STALE_EPOCH {
		t.Fatalf("stale epoch: %v", frames)
	}
	store.turn = false
	frames = fetchAll(t, client, &runtimev1.WorkspaceFetchPlanRequestV1{Claim: claim(2)})
	if len(frames) != 1 || frames[0].GetRefusal().GetCode() != runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_NOT_A_WORKSPACE_TURN {
		t.Fatalf("not a workspace turn: %v", frames)
	}
}

func TestFetchEndsWithARefusalWhenTheStoredSegmentIsCorrupt(t *testing.T) {
	store := newStore()
	store.corrupt = true
	client := dial(t, store, verifier{})
	frames := fetchAll(t, client, &runtimev1.WorkspaceFetchPlanRequestV1{Claim: claim(2)})
	last := frames[len(frames)-1]
	if last.GetRefusal().GetCode() != runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_CHECKSUM_MISMATCH {
		t.Fatalf("want checksum refusal last, got %v", last)
	}
	for _, frame := range frames {
		if frame.GetSegmentEnd() != nil {
			t.Fatal("a corrupt segment must never be closed as complete")
		}
	}
}

func TestEveryCallRefusesAStaleFenceBeforeTheStore(t *testing.T) {
	store := newStore()
	client := dial(t, store, verifier{err: runtimedomain.ErrStaleFence})
	frames := fetchAll(t, client, &runtimev1.WorkspaceFetchPlanRequestV1{Claim: claim(2)})
	if frames[0].GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE {
		t.Fatalf("fetch: %v", frames)
	}
	old := cloudSHA
	response, err := client.WorkspacePublish(context.Background(), publishRequest(2, &old))
	if err != nil || response.GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE {
		t.Fatalf("publish: %v %v", response, err)
	}
	if len(store.claims) != 0 {
		t.Fatal("the store was asked under a stale fence")
	}
}

func TestANilStoreIsFeatureUnavailable(t *testing.T) {
	client := dial(t, nil, verifier{})
	frames := fetchAll(t, client, &runtimev1.WorkspaceFetchPlanRequestV1{Claim: claim(2)})
	if frames[0].GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY {
		t.Fatalf("fetch: %v", frames)
	}
	old := cloudSHA
	response, _ := client.WorkspacePublish(context.Background(), publishRequest(2, &old))
	if response.GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY {
		t.Fatalf("publish: %v", response)
	}
}

func upload(t *testing.T, client runtimev1.WorkspaceStoreServiceClient, epoch uint64, content []byte, declared uint64) *runtimev1.WorkspaceUploadResponseV1 {
	t.Helper()
	stream, err := client.WorkspaceUpload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&runtimev1.WorkspaceUploadFrameV1{Frame: &runtimev1.WorkspaceUploadFrameV1_Header{Header: &runtimev1.WorkspaceUploadHeaderV1{Claim: claim(epoch), Sha256: digest(content), Bytes: declared}}}); err != nil {
		t.Fatal(err)
	}
	for rest := content; len(rest) > 0; {
		size := min(workspace.ChunkBytes, len(rest))
		if err := stream.Send(&runtimev1.WorkspaceUploadFrameV1{Frame: &runtimev1.WorkspaceUploadFrameV1_Chunk{Chunk: rest[:size]}}); err != nil {
			break
		}
		rest = rest[size:]
	}
	response, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func publishRequest(epoch uint64, old *string) *runtimev1.WorkspacePublishRequestV1 {
	return &runtimev1.WorkspacePublishRequestV1{
		Claim: claim(epoch), Kind: "COMPACT", SegmentSha256: digest([]byte("segment")), Bytes: 7, ObjectFormat: "sha1",
		Refs: []*runtimev1.WorkspaceRefUpdateV1{{Name: "refs/elitea/cloud", Old: old, New: nextSHA}},
	}
}

func TestUploadThenPublishMovesTheCloudRefWithCompareAndSet(t *testing.T) {
	store := newStore()
	client := dial(t, store, verifier{})
	segment := bytes.Repeat([]byte("x"), 100_000)
	if response := upload(t, client, 2, segment, uint64(len(segment))); response.GetRejection() != nil || response.GetRefusal() != nil || response.GetExists() {
		t.Fatalf("upload: %v", response)
	}
	if !bytes.Equal(store.uploaded[digest(segment)], segment) {
		t.Fatal("the store did not get the bytes")
	}
	if response := upload(t, client, 2, segment, uint64(len(segment))); !response.GetExists() {
		t.Fatal("a second upload of the same digest reports exists")
	}
	store.uploaded[digest([]byte("segment"))] = []byte("segment")
	old := cloudSHA
	response, err := client.WorkspacePublish(context.Background(), publishRequest(2, &old))
	if err != nil || response.GetSeq() != 4 || response.GetRefs()[0].GetSha() != nextSHA {
		t.Fatalf("publish: %v %v", response, err)
	}
	// The same expected old value again is a conflict that names the current ref.
	response, _ = client.WorkspacePublish(context.Background(), publishRequest(2, &old))
	if response.GetRefusal().GetCode() != runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_REF_CONFLICT || response.GetRefusal().GetCurrent()[0].GetSha() != nextSHA {
		t.Fatalf("conflict: %v", response)
	}
	// A stale epoch moves nothing.
	response, _ = client.WorkspacePublish(context.Background(), publishRequest(1, &old))
	if response.GetRefusal().GetCode() != runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_STALE_EPOCH || len(store.publishes) != 1 {
		t.Fatalf("stale: %v", response)
	}
}

func TestUploadRefusesAStreamLongerThanDeclared(t *testing.T) {
	store := newStore()
	client := dial(t, store, verifier{})
	segment := bytes.Repeat([]byte("y"), 70_000)
	response := upload(t, client, 2, segment, 40_000)
	if response.GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION {
		t.Fatalf("want malformed, got %v", response)
	}
	if len(store.uploaded) != 0 {
		t.Fatal("a malformed upload was stored")
	}
}

func TestPublishRefusesAnyRefButCloudWithoutAskingTheStore(t *testing.T) {
	store := newStore()
	client := dial(t, store, verifier{})
	request := publishRequest(2, nil)
	request.Refs[0].Name = "refs/elitea/handoff/1"
	response, _ := client.WorkspacePublish(context.Background(), request)
	if response.GetRefusal().GetCode() != runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_REF_NOT_ALLOWED {
		t.Fatalf("got %v", response)
	}
	request = publishRequest(2, nil)
	request.ObjectFormat = "sha256"
	response, _ = client.WorkspacePublish(context.Background(), request)
	if response.GetRefusal().GetCode() != runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_UNSUPPORTED_OBJECT_FORMAT {
		t.Fatalf("got %v", response)
	}
	if len(store.claims) != 0 {
		t.Fatal("the store was asked")
	}
}

func diffFrames(t *testing.T, client runtimev1.WorkspaceStoreServiceClient) []*runtimev1.WorkspacePullRequestDiffFrameV1 {
	t.Helper()
	stream, err := client.WorkspacePullRequestDiff(context.Background(), &runtimev1.WorkspacePullRequestDiffRequestV1{Claim: claim(2)})
	if err != nil {
		t.Fatal(err)
	}
	var frames []*runtimev1.WorkspacePullRequestDiffFrameV1
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
}

func TestPullRequestDiffStreamsTheHeaderAndTheDiff(t *testing.T) {
	store := newStore()
	client := dial(t, store, verifier{})
	if frames := diffFrames(t, client); frames[0].GetRefusal().GetCode() != runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_NO_UPSTREAM {
		t.Fatalf("no upstream: %v", frames)
	}
	content := blobs{}
	big := strings.Repeat("a line of text\n", 5_000)
	store.source = workspaceruntime.PullRequestSource{
		Upstream:     workspaceruntime.UpstreamInfo{SHA: upstreamC, RemoteURL: "https://user:token@github.com/org/repo.git", Branch: "main"},
		CloudSHA:     nextSHA,
		UpstreamTree: workspaceruntime.Tree{"a.txt": {Mode: workspaceruntime.ModeFile, OID: content.add("old\n")}},
		CloudTree: workspaceruntime.Tree{
			"a.txt":   {Mode: workspaceruntime.ModeFile, OID: content.add("new\n")},
			"big.txt": {Mode: workspaceruntime.ModeFile, OID: content.add(big)},
		},
		Blobs: content,
	}
	frames := diffFrames(t, client)
	header := frames[0].GetHeader()
	if header == nil || header.GetUpstreamSha() != upstreamC || header.GetUpstreamBranch() != "main" || header.GetFiles() != 2 ||
		header.GetRemoteUrl() != "https://github.com/org/repo.git" {
		t.Fatalf("header %v", frames[0])
	}
	var text []byte
	for _, frame := range frames[1:] {
		text = append(text, frame.GetChunk()...)
	}
	if uint64(len(text)) != header.GetBytes() || digest(text) != header.GetDiffSha256() || !strings.Contains(string(text), "-old\n+new\n") {
		t.Fatal("the diff did not arrive intact")
	}
	store.source.CloudTree["logo.png"] = workspaceruntime.TreeFile{Mode: workspaceruntime.ModeFile, OID: content.add("\x00PNG")}
	if frames := diffFrames(t, client); frames[0].GetRefusal().GetCode() != runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_DIFF_BINARY {
		t.Fatalf("binary: %v", frames)
	}
}

func TestUpstreamTrailerParsing(t *testing.T) {
	info, ok := workspaceruntime.ParseUpstreamTrailer("Elitea upstream\n\nElitea-Upstream: " + upstreamC + " https://x:y@github.com/o/r.git feature/one\n")
	if !ok || info.SHA != upstreamC || info.Branch != "feature/one" || info.RemoteURL != "https://github.com/o/r.git" {
		t.Fatalf("%+v %v", info, ok)
	}
	if _, ok := workspaceruntime.ParseUpstreamTrailer("no trailer"); ok {
		t.Fatal("parsed nothing")
	}
	if got := workspaceruntime.StripURLCredentials("git@github.com:o/r.git"); got != "github.com:o/r.git" {
		t.Fatal(got)
	}
}
