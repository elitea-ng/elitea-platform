package azure

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// Config holds Azure Blob-specific configuration.
type Config struct {
	Account       string
	Key           string
	ContainerName string // single container used as root

	// Endpoint overrides the default public-cloud service URL
	// (https://{Account}.blob.core.windows.net/) verbatim. Set it to reach
	// Azurite or a non-public-cloud deployment.
	Endpoint string

	// KeyPrefix namespaces every ObjectStore key under ContainerName; passed
	// through to ObjectRef.StorageKey/BucketPrefix. Optional, empty by
	// default.
	KeyPrefix string
}

// Backend implements storage.ObjectStore using Azure Blob Storage.
// All objects live in a single container with blob name: {projectID}/{bucketName}/{key}
type Backend struct {
	client        *azblob.Client
	containerName string
	keyPrefix     string

	// usesSharedKey records which credential path New took. blob.Client's
	// GetSASURL requires a shared key; a token-credential (workload
	// identity) client must mint a user-delegation SAS instead, which
	// Azurite does not support. Capabilities() (S3) reads this.
	usesSharedKey bool
	// token is the token credential of the workload-identity path (nil with a
	// shared key). Copy authorizes its copy source with it.
	token azcore.TokenCredential
}

var _ storage.ObjectStore = (*Backend)(nil)

// New creates an Azure Blob Backend. When cfg.Key is set it authenticates
// with a shared-key credential; otherwise it uses DefaultAzureCredential,
// enabling workload identity.
func New(_ context.Context, cfg Config) (*Backend, error) {
	serviceURL := cfg.Endpoint
	if serviceURL == "" {
		serviceURL = fmt.Sprintf("https://%s.blob.core.windows.net/", cfg.Account)
	}

	usesSharedKey := cfg.Key != ""
	var client *azblob.Client
	var token azcore.TokenCredential
	if usesSharedKey {
		cred, err := azblob.NewSharedKeyCredential(cfg.Account, cfg.Key)
		if err != nil {
			return nil, fmt.Errorf("storage/azure: credential: %w", err)
		}
		client, err = azblob.NewClientWithSharedKeyCredential(serviceURL, cred, nil)
		if err != nil {
			return nil, fmt.Errorf("storage/azure: client: %w", err)
		}
	} else {
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("storage/azure: default credential: %w", err)
		}
		client, err = azblob.NewClient(serviceURL, cred, nil)
		if err != nil {
			return nil, fmt.Errorf("storage/azure: client: %w", err)
		}
		token = cred
	}
	return &Backend{
		client:        client,
		containerName: cfg.ContainerName,
		keyPrefix:     cfg.KeyPrefix,
		usesSharedKey: usesSharedKey,
		token:         token,
	}, nil
}

func (b *Backend) containerClient() *container.Client {
	return b.client.ServiceClient().NewContainerClient(b.containerName)
}

// Capabilities reports NativeMultipart alongside Presign, not unconditionally
// true. PresignPart needs a SAS URL exactly like PresignGet/PresignPut, and
// GetSASURL only works with a shared-key credential — a user-delegation SAS
// could in principle cover the token-credential case too, but Azurite (this
// plan's own local/CI emulator, S4) does not issue user-delegation keys, so
// that path could not be exercised by the conformance suite even if built.
func (b *Backend) Capabilities() storage.Capabilities {
	return storage.Capabilities{Presign: b.usesSharedKey, NativeMultipart: b.usesSharedKey, ServerSideCopy: true}
}

func (b *Backend) Put(ctx context.Context, ref storage.ObjectRef, body io.Reader, opts storage.PutOptions) (storage.ObjectInfo, error) {
	blobClient := b.containerClient().NewBlockBlobClient(ref.StorageKey(b.keyPrefix))
	uploadOpts := &azblob.UploadStreamOptions{
		// The library defaults (1 MiB block, concurrency 1) cap an object at
		// 50000 blocks x 1 MiB =~ 48.8 GiB and serialise every upload. 8 MiB
		// raises the ceiling to about 390 GiB.
		BlockSize:   8 << 20,
		Concurrency: 4,
	}
	if opts.ContentType != "" {
		contentType := opts.ContentType
		uploadOpts.HTTPHeaders = &blob.HTTPHeaders{BlobContentType: &contentType}
	}
	if len(opts.Metadata) > 0 {
		meta := make(map[string]*string, len(opts.Metadata))
		for k, v := range opts.Metadata {
			v := v
			meta[k] = &v
		}
		uploadOpts.Metadata = meta
	}
	// UploadStreamResponse (a block-list commit response) carries no
	// size/content-length field at all — confirmed by reading the SDK's own
	// response type. The actual number of bytes read from body is counted
	// here instead, the only source of truth available for a streamed,
	// size-unknown body. Same defect S19's conformance suite found in the
	// S3 backend's non-seekable Put path — see its own doc comment for the
	// full story (every real multipart-form upload was silently reporting
	// size_bytes: 0 and recording byte_length 0 into elitea_storage.objects).
	counted := &countingReader{r: body}
	resp, err := blobClient.UploadStream(ctx, counted, uploadOpts)
	if err != nil {
		return storage.ObjectInfo{}, mapAzureError(err, "put object")
	}
	return storage.ObjectInfo{
		Key:          ref.Key(),
		Size:         counted.n,
		ContentType:  opts.ContentType,
		ETag:         derefETagString(resp.ETag),
		LastModified: derefTime(resp.LastModified),
	}, nil
}

// countingReader tallies bytes read from an underlying, size-unknown
// io.Reader — see Put's non-seekable-body upload above.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (b *Backend) Get(ctx context.Context, ref storage.ObjectRef, rng *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	blobClient := b.containerClient().NewBlobClient(ref.StorageKey(b.keyPrefix))
	opts := &blob.DownloadStreamOptions{}
	if rng != nil {
		count := int64(0)
		if rng.End >= 0 {
			count = rng.End - rng.Start + 1
		}
		opts.Range = blob.HTTPRange{Offset: rng.Start, Count: count}
	}
	resp, err := blobClient.DownloadStream(ctx, opts)
	if err != nil {
		return nil, storage.ObjectInfo{}, mapAzureError(err, "get object")
	}
	// ContentLength is the length of the RANGE on a ranged download. The
	// whole blob's size comes from the Content-Range header Azure sends with
	// a 206, which a caller needs to build its own Content-Range.
	size := derefInt64(resp.ContentLength)
	total := size
	if parsed, ok := totalFromContentRange(derefString(resp.ContentRange)); ok {
		total = parsed
	}
	info := storage.ObjectInfo{
		Key:          ref.Key(),
		Size:         size,
		TotalSize:    total,
		LastModified: derefTime(resp.LastModified),
		ContentType:  derefString(resp.ContentType),
		ETag:         derefETagString(resp.ETag),
	}
	return resp.Body, info, nil
}

// totalFromContentRange reads the complete-length field out of a
// "bytes <start>-<end>/<total>" header. A total of "*" is unknown and
// returns false, as does any header this parser does not recognise.
func totalFromContentRange(header string) (int64, bool) {
	slash := strings.LastIndex(header, "/")
	if slash < 0 {
		return 0, false
	}
	total, err := strconv.ParseInt(strings.TrimSpace(header[slash+1:]), 10, 64)
	if err != nil || total < 0 {
		return 0, false
	}
	return total, true
}

func (b *Backend) Stat(ctx context.Context, ref storage.ObjectRef) (storage.ObjectInfo, error) {
	blobClient := b.containerClient().NewBlobClient(ref.StorageKey(b.keyPrefix))
	props, err := blobClient.GetProperties(ctx, nil)
	if err != nil {
		return storage.ObjectInfo{}, mapAzureError(err, "stat object")
	}
	return storage.ObjectInfo{
		Key:          ref.Key(),
		Size:         derefInt64(props.ContentLength),
		LastModified: derefTime(props.LastModified),
		ContentType:  derefString(props.ContentType),
		ETag:         derefETagString(props.ETag),
	}, nil
}

// Delete swallows BlobNotFound and ContainerNotFound and returns nil,
// matching the interface's idempotency rule.
func (b *Backend) Delete(ctx context.Context, ref storage.ObjectRef) error {
	_, err := b.containerClient().NewBlobClient(ref.StorageKey(b.keyPrefix)).Delete(ctx, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ContainerNotFound) {
			return nil
		}
		return mapAzureError(err, "delete object")
	}
	return nil
}

// DeleteBatch has no bulk API in this client library — issue Delete calls
// sequentially and report each outcome individually.
func (b *Backend) DeleteBatch(ctx context.Context, refs []storage.ObjectRef) (storage.BatchResult, error) {
	var result storage.BatchResult
	for _, ref := range refs {
		if err := b.Delete(ctx, ref); err != nil {
			result.Failed = append(result.Failed, storage.BatchError{Key: ref.Key(), Err: err})
			continue
		}
		result.Deleted = append(result.Deleted, ref.Key())
	}
	return result, nil
}

func (b *Backend) List(ctx context.Context, q storage.ListQuery) (storage.ListPage, error) {
	if err := storage.ValidateKeyPrefix(q.KeyPrefix); err != nil {
		return storage.ListPage{}, err
	}

	basePrefix := q.Bucket.BucketPrefix(b.keyPrefix)
	prefix := basePrefix + q.KeyPrefix

	var maxResults *int32
	if q.MaxKeys > 0 {
		mk := q.MaxKeys
		if mk > 1000 {
			mk = 1000
		}
		maxResults = &mk
	}
	var marker *string
	if q.ContinuationToken != "" {
		marker = &q.ContinuationToken
	}

	page := storage.ListPage{}
	if q.Delimiter != "" {
		pager := b.containerClient().NewListBlobsHierarchyPager(q.Delimiter, &container.ListBlobsHierarchyOptions{
			Prefix:     &prefix,
			Marker:     marker,
			MaxResults: maxResults,
		})
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return storage.ListPage{}, mapAzureError(err, "list objects")
		}
		for _, item := range resp.Segment.BlobItems {
			page.Objects = append(page.Objects, blobItemToObjectInfo(item, basePrefix))
		}
		for _, bp := range resp.Segment.BlobPrefixes {
			page.CommonPrefixes = append(page.CommonPrefixes, strings.TrimPrefix(derefString(bp.Name), basePrefix))
		}
		page.NextContinuationToken = derefString(resp.NextMarker)
		page.IsTruncated = page.NextContinuationToken != ""
		return page, nil
	}

	pager := b.containerClient().NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix:     &prefix,
		Marker:     marker,
		MaxResults: maxResults,
	})
	resp, err := pager.NextPage(ctx)
	if err != nil {
		return storage.ListPage{}, mapAzureError(err, "list objects")
	}
	for _, item := range resp.Segment.BlobItems {
		page.Objects = append(page.Objects, blobItemToObjectInfo(item, basePrefix))
	}
	page.NextContinuationToken = derefString(resp.NextMarker)
	page.IsTruncated = page.NextContinuationToken != ""
	return page, nil
}

func blobItemToObjectInfo(item *container.BlobItem, basePrefix string) storage.ObjectInfo {
	info := storage.ObjectInfo{Key: strings.TrimPrefix(derefString(item.Name), basePrefix)}
	if item.Properties != nil {
		info.Size = derefInt64(item.Properties.ContentLength)
		info.LastModified = derefTime(item.Properties.LastModified)
		info.ContentType = derefString(item.Properties.ContentType)
		info.ETag = derefETagString(item.Properties.ETag)
	}
	return info
}

func (b *Backend) PresignGet(ctx context.Context, ref storage.ObjectRef, ttl time.Duration) (string, error) {
	if !b.usesSharedKey {
		return "", storage.ErrNotSupported
	}
	blobClient := b.containerClient().NewBlobClient(ref.StorageKey(b.keyPrefix))
	presignedURL, err := blobClient.GetSASURL(sas.BlobPermissions{Read: true}, time.Now().Add(ttl), nil)
	if err != nil {
		return "", fmt.Errorf("storage/azure: presign get: %w", err)
	}
	return presignedURL, nil
}

// PresignPut's opts.ContentType, when set, is not covered by the SAS
// signature — like S3, a presigned PUT does not enforce media type at the
// storage layer. S15 verifies it on commit.
func (b *Backend) PresignPut(ctx context.Context, ref storage.ObjectRef, ttl time.Duration, opts storage.PutOptions) (string, error) {
	if !b.usesSharedKey {
		return "", storage.ErrNotSupported
	}
	blobClient := b.containerClient().NewBlockBlobClient(ref.StorageKey(b.keyPrefix))
	presignedURL, err := blobClient.GetSASURL(sas.BlobPermissions{Write: true, Create: true}, time.Now().Add(ttl), nil)
	if err != nil {
		return "", fmt.Errorf("storage/azure: presign put: %w", err)
	}
	return presignedURL, nil
}

// StartMultipart has no server round trip on Azure: blocks are staged
// directly against the target blob path with client-chosen block IDs and
// assembled later via CommitBlockList. The returned UploadID is a random
// correlation token used only to derive deterministic block IDs in
// PresignPart/CompleteMultipart below; Azure never sees it.
func (b *Backend) StartMultipart(ctx context.Context, ref storage.ObjectRef, opts storage.PutOptions) (storage.UploadID, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("storage/azure: start multipart: %w", err)
	}
	return storage.UploadID(hex.EncodeToString(buf)), nil
}

// blockID deterministically derives a block ID from id and part. Azure
// requires every block ID committed to one blob to be the same length once
// base64-decoded; id is always a fixed-length 32 hex-character string and
// part is always formatted to 10 digits, so the pre-encoding length is
// constant across every part of a given upload.
func blockID(id storage.UploadID, part int32) string {
	raw := fmt.Sprintf("%s-%010d", id, part)
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

func (b *Backend) PresignPart(ctx context.Context, ref storage.ObjectRef, id storage.UploadID, part int32, ttl time.Duration) (string, error) {
	if !b.usesSharedKey {
		return "", storage.ErrNotSupported
	}
	blobClient := b.containerClient().NewBlockBlobClient(ref.StorageKey(b.keyPrefix))
	sasURL, err := blobClient.GetSASURL(sas.BlobPermissions{Write: true, Create: true}, time.Now().Add(ttl), nil)
	if err != nil {
		return "", fmt.Errorf("storage/azure: presign part: %w", err)
	}
	sep := "&"
	if !strings.Contains(sasURL, "?") {
		sep = "?"
	}
	return sasURL + sep + "comp=block&blockid=" + url.QueryEscape(blockID(id, part)), nil
}

func (b *Backend) CompleteMultipart(ctx context.Context, ref storage.ObjectRef, id storage.UploadID, parts []storage.Part) (storage.ObjectInfo, error) {
	sorted := append([]storage.Part(nil), parts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })

	blockIDs := make([]string, len(sorted))
	for i, p := range sorted {
		blockIDs[i] = blockID(id, p.Number)
	}

	blobClient := b.containerClient().NewBlockBlobClient(ref.StorageKey(b.keyPrefix))
	resp, err := blobClient.CommitBlockList(ctx, blockIDs, nil)
	if err != nil {
		return storage.ObjectInfo{}, mapAzureError(err, "complete multipart")
	}
	return storage.ObjectInfo{
		Key:          ref.Key(),
		ETag:         derefETagString(resp.ETag),
		LastModified: derefTime(resp.LastModified),
	}, nil
}

// AbortMultipart is a no-op: Azure has no explicit abort API for staged
// blocks. Uncommitted blocks are automatically garbage-collected roughly a
// week after their last staging call regardless of any explicit action —
// this is the deliberate resolution S14's coarse provider-native lifecycle
// rule reached for this backend, not an oversight. Unlike S3/GCS, the data
// used elsewhere in this backend (github.com/Azure/azure-sdk-for-go/sdk/
// storage/azblob, azure.Config's Account/Key/ContainerName/Endpoint) has no
// blob-lifecycle-management call at all — lifecycle policies are an Azure
// Resource Manager (management-plane) resource, reached through a
// different SDK (armstorage.ManagementPoliciesClient) that needs a
// subscription ID, resource group, and storage-account resource ID this
// Config does not carry, and Azurite (this stack's Azure emulator) exposes
// no ARM endpoint at all — architecturally untestable here, not just an
// emulator gap. Since the default GC already delivers the desired
// behavior, adding that management-plane dependency purely to make it
// explicit was judged not worth the added configuration surface. See
// docs/plans/storage-migration-plan.md's S14 section for this tradeoff.
func (b *Backend) AbortMultipart(ctx context.Context, ref storage.ObjectRef, id storage.UploadID) error {
	return nil
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt64(i *int64) int64 {
	if i == nil {
		return 0
	}
	return *i
}

func derefETagString(e *azcore.ETag) string {
	if e == nil {
		return ""
	}
	return string(*e)
}

// mapAzureError maps not-found and access-denied blob error codes to the
// shared sentinels, wrapping the original error so errors.Is still finds it.
func mapAzureError(err error, op string) error {
	if bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ContainerNotFound) {
		return fmt.Errorf("storage/azure: %s: %w", op, storage.ErrNotFound)
	}
	if bloberror.HasCode(err, bloberror.AuthenticationFailed, bloberror.AuthorizationFailure, bloberror.AuthorizationPermissionMismatch) {
		return fmt.Errorf("storage/azure: %s: %w", op, storage.ErrAccessDenied)
	}
	return fmt.Errorf("storage/azure: %s: %w", op, err)
}

// DefaultEncryptionConfirmed: Azure Storage encrypts every blob at rest and
// the encryption cannot be disabled (ADR-0034 decision 4).
func (b *Backend) DefaultEncryptionConfirmed(context.Context) (bool, error) { return true, nil }

var _ storage.ObjectCopier = (*Backend)(nil)

// copyPollInterval and copyWait bound the wait for an Azure copy, which the
// service may finish asynchronously.
const (
	copyPollInterval = 200 * time.Millisecond
	copyWait         = 2 * time.Minute
)

// storageScope is the OAuth scope of Azure Storage, for a copy source token.
const storageScope = "https://storage.azure.com/.default"

// maxSyncCopyBytes is Azure's limit for the synchronous Copy Blob From URL.
const maxSyncCopyBytes = 256 << 20

// Copy is Azure Copy Blob inside the backend's container.
//
//   - Shared key: an asynchronous Copy Blob; the same-account source is
//     authorized by the request's own key. A copy the service answers as
//     pending is polled until it succeeds, fails or times out.
//   - Workload identity (Entra): the bare URL of a private blob is refused
//     (403 CannotVerifyCopySource), so the copy is a synchronous Copy Blob
//     From URL with CopySourceAuthorization "Bearer <token>" for the storage
//     scope. A source above its 256 MiB limit answers ErrNotSupported.
//
// A refused copy source (403, CannotVerifyCopySource, AuthorizationFailure)
// answers storage.ErrNotSupported, so a caller with a read-back fallback
// (the workspace store's promote) degrades instead of failing.
func (b *Backend) Copy(ctx context.Context, src, dst storage.ObjectRef) (storage.ObjectInfo, error) {
	containerClient := b.containerClient()
	source := containerClient.NewBlobClient(src.StorageKey(b.keyPrefix)).URL()
	target := containerClient.NewBlobClient(dst.StorageKey(b.keyPrefix))
	if b.token != nil {
		info, err := b.Stat(ctx, src)
		if err != nil {
			return storage.ObjectInfo{}, err
		}
		if info.Size > maxSyncCopyBytes {
			return storage.ObjectInfo{}, storage.ErrNotSupported
		}
		authorization, err := copySourceAuthorization(ctx, b.token)
		if err != nil {
			// No token, no authorized copy: the caller falls back to the
			// read-back, as for a refused copy source.
			return storage.ObjectInfo{}, fmt.Errorf("storage/azure: copy source token: %w: %w", storage.ErrNotSupported, err)
		}
		if _, err := target.CopyFromURL(ctx, source, &blob.CopyFromURLOptions{CopySourceAuthorization: &authorization}); err != nil {
			return storage.ObjectInfo{}, copyError(err)
		}
		return b.Stat(ctx, dst)
	}
	started, err := target.StartCopyFromURL(ctx, source, nil)
	if err != nil {
		return storage.ObjectInfo{}, copyError(err)
	}
	status := blob.CopyStatusType("")
	if started.CopyStatus != nil {
		status = *started.CopyStatus
	}
	deadline := time.Now().Add(copyWait)
	for status == blob.CopyStatusTypePending {
		if time.Now().After(deadline) {
			return storage.ObjectInfo{}, fmt.Errorf("storage/azure: copy object: still pending after %s", copyWait)
		}
		select {
		case <-ctx.Done():
			return storage.ObjectInfo{}, ctx.Err()
		case <-time.After(copyPollInterval):
		}
		props, err := target.GetProperties(ctx, nil)
		if err != nil {
			return storage.ObjectInfo{}, mapAzureError(err, "copy object status")
		}
		if props.CopyStatus != nil {
			status = *props.CopyStatus
		}
	}
	if status != blob.CopyStatusTypeSuccess && status != "" {
		return storage.ObjectInfo{}, fmt.Errorf("storage/azure: copy object: status %s", status)
	}
	return b.Stat(ctx, dst)
}

// copySourceAuthorization is the Bearer value of a copy source token.
func copySourceAuthorization(ctx context.Context, credential azcore.TokenCredential) (string, error) {
	token, err := credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{storageScope}})
	if err != nil {
		return "", err
	}
	return "Bearer " + token.Token, nil
}

// copyError maps a refused copy source to storage.ErrNotSupported (the
// caller falls back to reading the bytes back), and anything else as usual.
func copyError(err error) error {
	var response *azcore.ResponseError
	if bloberror.HasCode(err, bloberror.CannotVerifyCopySource, bloberror.AuthorizationFailure,
		bloberror.AuthorizationPermissionMismatch) ||
		(errors.As(err, &response) && response.StatusCode == 403) {
		return fmt.Errorf("storage/azure: copy source refused: %w: %w", storage.ErrNotSupported, err)
	}
	return mapAzureError(err, "copy object")
}
