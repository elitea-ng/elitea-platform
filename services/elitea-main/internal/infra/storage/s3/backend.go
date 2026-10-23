package s3

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// Config holds S3-specific configuration.
type Config struct {
	Endpoint       string
	AccessKey      string
	SecretKey      string
	Region         string
	ForcePathStyle bool

	// Bucket is the single physical S3 bucket the ObjectStore methods
	// operate against — every object is namespaced within it via key-prefix
	// multiplexing (ObjectRef.StorageKey), not one physical bucket per
	// (project, bucket) pair.
	Bucket string

	// KeyPrefix namespaces every ObjectStore key under Bucket; passed
	// through to ObjectRef.StorageKey/BucketPrefix. Optional, empty by
	// default.
	KeyPrefix string

	// NoPresign turns presigning off: Capabilities reports neither Presign
	// nor NativeMultipart, so every client upload and download goes through
	// Main's streaming facade. For an endpoint only Main can reach, such as
	// an in-cluster RustFS, whose presigned URLs name a host the desktop
	// cannot resolve.
	NoPresign bool
}

// Backend implements storage.ObjectStore using S3-compatible APIs (AWS S3,
// MinIO, RustFS). Every object lives in one physical bucket (Config.Bucket),
// namespaced by ObjectRef.StorageKey.
type Backend struct {
	client   *s3.Client
	uploader *transfermanager.Client // non-seekable-body Put path only; see Put

	bucket    string
	keyPrefix string
	noPresign bool
}

var _ storage.ObjectStore = (*Backend)(nil)

// New creates an S3 Backend.
func New(ctx context.Context, cfg Config) (*Backend, error) {
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	if cfg.AccessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AccessKey, cfg.SecretKey, "",
		)))
	}
	// When AccessKey is empty, do not force static (empty) credentials —
	// let LoadDefaultConfig resolve IRSA, instance, and environment
	// credentials instead.

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("storage/s3: load config: %w", err)
	}

	clientOpts := []func(*s3.Options){}
	if cfg.Endpoint != "" {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = cfg.ForcePathStyle
		})
	}

	client := s3.NewFromConfig(awsCfg, clientOpts...)
	return &Backend{
		client: client,
		// The transfer manager does not inherit the client's checksum
		// setting: unset, it sends CRC32 on every part. Hand it the resolved
		// one, so AWS_REQUEST_CHECKSUM_CALCULATION=when_required reaches the
		// multipart path too (RustFS 1.0.0-beta.8 refuses a checksummed
		// UploadPart into a bucket with default SSE).
		uploader: transfermanager.New(client, func(o *transfermanager.Options) {
			o.RequestChecksumCalculation = awsCfg.RequestChecksumCalculation
		}),
		bucket:    cfg.Bucket,
		keyPrefix: cfg.KeyPrefix,
		noPresign: cfg.NoPresign,
	}, nil
}

// ConfigureRetentionLifecycle sets a coarse, one-time, provider-native
// bucket lifecycle rule aborting incomplete multipart uploads left dangling
// by a crashed/abandoned client after 7 days (S3 bills for their storage
// indefinitely otherwise; the retention sweeper — internal/runtimecomposition
// — has no way to discover them at all, since they are not objects; actual
// object/bucket retention stays a Go-owned ledger, not a provider lifecycle
// rule — this is the one deliberate exception).
//
// Deliberately not called from New(): New() must stay a cheap,
// non-network-dialing constructor — cmd/elitea-main's
// TestArtifactNewObjectStoreDispatchesPerBackend asserts exactly that for
// every backend. This is a separate, explicit startup step, alongside
// main.go's own objectStoreReadinessProbe, not folded into construction.
// Confirmed empirically against RustFS (this stack's S3 emulator): the rule
// sets and round-trips correctly (see lifecycle_test.go).
func (b *Backend) ConfigureRetentionLifecycle(ctx context.Context) error {
	if _, err := b.client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(b.bucket),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{
			Rules: []types.LifecycleRule{
				{
					ID:     aws.String("elitea-abort-incomplete-multipart-uploads"),
					Status: types.ExpirationStatusEnabled,
					Filter: &types.LifecycleRuleFilter{Prefix: aws.String("")},
					AbortIncompleteMultipartUpload: &types.AbortIncompleteMultipartUpload{
						DaysAfterInitiation: aws.Int32(7),
					},
				},
			},
		},
	}); err != nil {
		return fmt.Errorf("storage/s3: configure incomplete-multipart-upload lifecycle rule: %w", err)
	}
	return nil
}

func (b *Backend) Capabilities() storage.Capabilities {
	// Native multipart is offered only through presigned part URLs.
	return storage.Capabilities{Presign: !b.noPresign, NativeMultipart: !b.noPresign, ServerSideCopy: true, ChecksumSHA256: true}
}

func (b *Backend) Put(ctx context.Context, ref storage.ObjectRef, body io.Reader, opts storage.PutOptions) (storage.ObjectInfo, error) {
	key := ref.StorageKey(b.keyPrefix)

	if seeker, ok := body.(io.ReadSeeker); ok {
		size := opts.ContentLength
		if size < 0 {
			var err error
			size, err = seekableSize(seeker)
			if err != nil {
				return storage.ObjectInfo{}, fmt.Errorf("storage/s3: measure seekable body: %w", err)
			}
		}
		input := &s3.PutObjectInput{
			Bucket:        aws.String(b.bucket),
			Key:           aws.String(key),
			Body:          seeker,
			ContentLength: aws.Int64(size),
		}
		if opts.ContentType != "" {
			input.ContentType = aws.String(opts.ContentType)
		}
		if len(opts.Metadata) > 0 {
			input.Metadata = opts.Metadata
		}
		if len(opts.ChecksumSHA256) == sha256.Size {
			input.ChecksumSHA256 = aws.String(base64.StdEncoding.EncodeToString(opts.ChecksumSHA256))
		}
		out, err := b.client.PutObject(ctx, input)
		if err != nil {
			return storage.ObjectInfo{}, mapS3Error(err, "put object")
		}
		return storage.ObjectInfo{
			Key:         ref.Key(),
			Size:        size,
			ContentType: opts.ContentType,
			ETag:        strings.Trim(aws.ToString(out.ETag), `"`),
		}, nil
	}

	// Non-seekable bodies (S9 passes a *multipart.Part) fail SigV4's
	// RewindStream before the request reaches the network, over plain HTTP —
	// route them through the transfer manager's multipart uploader instead
	// of PutObject. UploadObjectOutput carries no size/content-length field
	// at all (confirmed by reading the SDK's own struct — checksums, ETag,
	// upload ID, parts, nothing byte-count-shaped), so the actual number of
	// bytes read from body is counted here instead — the only source of
	// truth available once ContentLength was unknown going in. Found
	// empirically by S19's conformance suite: every real multipart-form
	// upload through the production UploadObject handler (objects.go always
	// passes ContentLength: -1) was silently reporting size_bytes: 0 to
	// callers and recording byte_length 0 into elitea_storage.objects,
	// zeroing every size aggregate for every object actually uploaded this
	// way — the same class of defect S12 found in the metadata-write path
	// itself, this time in the size value written down that path.
	counted := &countingReader{r: body}
	uploadInput := &transfermanager.UploadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
		Body:   counted,
	}
	if opts.ContentType != "" {
		uploadInput.ContentType = aws.String(opts.ContentType)
	}
	if len(opts.Metadata) > 0 {
		uploadInput.Metadata = opts.Metadata
	}
	out, err := b.uploader.UploadObject(ctx, uploadInput)
	if err != nil {
		return storage.ObjectInfo{}, mapS3Error(err, "upload object")
	}
	return storage.ObjectInfo{
		Key:         ref.Key(),
		Size:        counted.n,
		ContentType: opts.ContentType,
		ETag:        strings.Trim(aws.ToString(out.ETag), `"`),
	}, nil
}

// countingReader tallies bytes read from an underlying, size-unknown
// io.Reader — see Put's non-seekable-body path above.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func seekableSize(s io.ReadSeeker) (int64, error) {
	current, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	end, err := s.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := s.Seek(current, io.SeekStart); err != nil {
		return 0, err
	}
	return end - current, nil
}

func (b *Backend) Get(ctx context.Context, ref storage.ObjectRef, rng *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	input := &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(ref.StorageKey(b.keyPrefix)),
	}
	if rng != nil {
		input.Range = aws.String(formatByteRange(*rng))
	}
	out, err := b.client.GetObject(ctx, input)
	if err != nil {
		return nil, storage.ObjectInfo{}, mapS3Error(err, "get object")
	}
	// ContentLength is the length of the RANGE on a ranged GET. The whole
	// object's size comes from the Content-Range header S3 sends with a 206,
	// which a caller needs to build its own Content-Range.
	size := aws.ToInt64(out.ContentLength)
	total := size
	if parsed, ok := totalFromContentRange(aws.ToString(out.ContentRange)); ok {
		total = parsed
	}
	info := storage.ObjectInfo{
		Key:          ref.Key(),
		Size:         size,
		TotalSize:    total,
		LastModified: aws.ToTime(out.LastModified),
		ContentType:  aws.ToString(out.ContentType),
		ETag:         strings.Trim(aws.ToString(out.ETag), `"`),
	}
	return out.Body, info, nil
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

func formatByteRange(r storage.ByteRange) string {
	if r.End < 0 {
		return fmt.Sprintf("bytes=%d-", r.Start)
	}
	return fmt.Sprintf("bytes=%d-%d", r.Start, r.End)
}

func (b *Backend) Stat(ctx context.Context, ref storage.ObjectRef) (storage.ObjectInfo, error) {
	out, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket:       aws.String(b.bucket),
		Key:          aws.String(ref.StorageKey(b.keyPrefix)),
		ChecksumMode: types.ChecksumModeEnabled,
	})
	if err != nil {
		return storage.ObjectInfo{}, mapS3Error(err, "stat object")
	}
	return storage.ObjectInfo{
		Key:          ref.Key(),
		Size:         aws.ToInt64(out.ContentLength),
		LastModified: aws.ToTime(out.LastModified),
		ContentType:  aws.ToString(out.ContentType),
		ETag:         strings.Trim(aws.ToString(out.ETag), `"`),
		DigestSHA256: fullObjectSHA256(out.ChecksumSHA256, out.ChecksumType),
	}, nil
}

// Delete is idempotent: S3 does not error on deleting an already-absent key,
// so no special-case mapping is needed here (contrast the gcs backend).
func (b *Backend) Delete(ctx context.Context, ref storage.ObjectRef) error {
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(ref.StorageKey(b.keyPrefix)),
	})
	if err != nil {
		return mapS3Error(err, "delete object")
	}
	return nil
}

func (b *Backend) DeleteBatch(ctx context.Context, refs []storage.ObjectRef) (storage.BatchResult, error) {
	var result storage.BatchResult
	for start := 0; start < len(refs); start += 1000 {
		end := start + 1000
		if end > len(refs) {
			end = len(refs)
		}
		chunk := refs[start:end]

		objs := make([]types.ObjectIdentifier, len(chunk))
		keyByStorageKey := make(map[string]string, len(chunk))
		for i, ref := range chunk {
			storageKey := ref.StorageKey(b.keyPrefix)
			objs[i] = types.ObjectIdentifier{Key: aws.String(storageKey)}
			keyByStorageKey[storageKey] = ref.Key()
		}

		// Quiet is deliberately left nil (not set to false — *bool with
		// Quiet: false does not compile). Omitting it selects verbose mode,
		// which is what populates Deleted[] below.
		out, err := b.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(b.bucket),
			Delete: &types.Delete{Objects: objs},
		})
		if err != nil {
			return result, mapS3Error(err, "delete batch")
		}
		for _, d := range out.Deleted {
			result.Deleted = append(result.Deleted, keyByStorageKey[aws.ToString(d.Key)])
		}
		for _, e := range out.Errors {
			result.Failed = append(result.Failed, storage.BatchError{
				Key: keyByStorageKey[aws.ToString(e.Key)],
				Err: fmt.Errorf("storage/s3: %s: %s", aws.ToString(e.Code), aws.ToString(e.Message)),
			})
		}
	}
	return result, nil
}

func (b *Backend) List(ctx context.Context, q storage.ListQuery) (storage.ListPage, error) {
	if err := storage.ValidateKeyPrefix(q.KeyPrefix); err != nil {
		return storage.ListPage{}, err
	}

	basePrefix := q.Bucket.BucketPrefix(b.keyPrefix)
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(basePrefix + q.KeyPrefix),
	}
	if q.Delimiter != "" {
		input.Delimiter = aws.String(q.Delimiter)
	}
	if q.MaxKeys > 0 {
		maxKeys := q.MaxKeys
		if maxKeys > 1000 {
			maxKeys = 1000
		}
		input.MaxKeys = aws.Int32(maxKeys)
	}
	if q.ContinuationToken != "" {
		input.ContinuationToken = aws.String(q.ContinuationToken)
	}

	out, err := b.client.ListObjectsV2(ctx, input)
	if err != nil {
		return storage.ListPage{}, mapS3Error(err, "list objects")
	}

	page := storage.ListPage{
		IsTruncated:           aws.ToBool(out.IsTruncated),
		NextContinuationToken: aws.ToString(out.NextContinuationToken),
	}
	for _, obj := range out.Contents {
		page.Objects = append(page.Objects, storage.ObjectInfo{
			Key:          strings.TrimPrefix(aws.ToString(obj.Key), basePrefix),
			Size:         aws.ToInt64(obj.Size),
			LastModified: aws.ToTime(obj.LastModified),
			ETag:         strings.Trim(aws.ToString(obj.ETag), `"`),
		})
	}
	for _, cp := range out.CommonPrefixes {
		page.CommonPrefixes = append(page.CommonPrefixes, strings.TrimPrefix(aws.ToString(cp.Prefix), basePrefix))
	}
	return page, nil
}

func (b *Backend) PresignGet(ctx context.Context, ref storage.ObjectRef, ttl time.Duration) (string, error) {
	if b.noPresign {
		return "", storage.ErrNotSupported
	}
	presigned, err := s3.NewPresignClient(b.client).PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(ref.StorageKey(b.keyPrefix)),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("storage/s3: presign get: %w", err)
	}
	return presigned.URL, nil
}

// PresignPut's ContentType, when set, is not part of the signature — a
// presigned PUT does not enforce media type at the storage layer. S15
// verifies it on commit instead.
func (b *Backend) PresignPut(ctx context.Context, ref storage.ObjectRef, ttl time.Duration, opts storage.PutOptions) (string, error) {
	if b.noPresign {
		return "", storage.ErrNotSupported
	}
	input := &s3.PutObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(ref.StorageKey(b.keyPrefix)),
	}
	if opts.ContentType != "" {
		input.ContentType = aws.String(opts.ContentType)
	}
	if len(opts.ChecksumSHA256) == sha256.Size {
		// X-Amz-Checksum-* is a required signed header: the client must
		// send this exact value, and the store refuses other bytes.
		input.ChecksumSHA256 = aws.String(base64.StdEncoding.EncodeToString(opts.ChecksumSHA256))
	}
	presigned, err := s3.NewPresignClient(b.client).PresignPutObject(ctx, input, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("storage/s3: presign put: %w", err)
	}
	return presigned.URL, nil
}

func (b *Backend) StartMultipart(ctx context.Context, ref storage.ObjectRef, opts storage.PutOptions) (storage.UploadID, error) {
	input := &s3.CreateMultipartUploadInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(ref.StorageKey(b.keyPrefix)),
	}
	if opts.ContentType != "" {
		input.ContentType = aws.String(opts.ContentType)
	}
	out, err := b.client.CreateMultipartUpload(ctx, input)
	if err != nil {
		return "", fmt.Errorf("storage/s3: start multipart: %w", err)
	}
	return storage.UploadID(aws.ToString(out.UploadId)), nil
}

func (b *Backend) PresignPart(ctx context.Context, ref storage.ObjectRef, id storage.UploadID, part int32, ttl time.Duration) (string, error) {
	if b.noPresign {
		return "", storage.ErrNotSupported
	}
	presigned, err := s3.NewPresignClient(b.client).PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(b.bucket),
		Key:        aws.String(ref.StorageKey(b.keyPrefix)),
		UploadId:   aws.String(string(id)),
		PartNumber: aws.Int32(part),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("storage/s3: presign part: %w", err)
	}
	return presigned.URL, nil
}

// CompleteMultipart requires parts in strictly ascending PartNumber order —
// S3 (confirmed against real RustFS) rejects an out-of-order list with
// InvalidPartOrder. Callers are not required to pass parts pre-sorted.
func (b *Backend) CompleteMultipart(ctx context.Context, ref storage.ObjectRef, id storage.UploadID, parts []storage.Part) (storage.ObjectInfo, error) {
	sorted := append([]storage.Part(nil), parts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })

	completed := make([]types.CompletedPart, len(sorted))
	for i, p := range sorted {
		completed[i] = types.CompletedPart{
			ETag:       aws.String(quoteETag(p.ETag)),
			PartNumber: aws.Int32(p.Number),
		}
	}
	out, err := b.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(b.bucket),
		Key:             aws.String(ref.StorageKey(b.keyPrefix)),
		UploadId:        aws.String(string(id)),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		return storage.ObjectInfo{}, fmt.Errorf("storage/s3: complete multipart: %w", err)
	}
	return storage.ObjectInfo{
		Key:  ref.Key(),
		ETag: strings.Trim(aws.ToString(out.ETag), `"`),
	}, nil
}

func (b *Backend) AbortMultipart(ctx context.Context, ref storage.ObjectRef, id storage.UploadID) error {
	_, err := b.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(b.bucket),
		Key:      aws.String(ref.StorageKey(b.keyPrefix)),
		UploadId: aws.String(string(id)),
	})
	if err != nil {
		return fmt.Errorf("storage/s3: abort multipart: %w", err)
	}
	return nil
}

// quoteETag ensures an ETag is wrapped in quotes, matching what
// CompleteMultipartUpload expects to receive back from a prior UploadPart
// response — S3 requires the exact quoted form.
func quoteETag(s string) string {
	if s == "" || strings.HasPrefix(s, `"`) {
		return s
	}
	return `"` + s + `"`
}

// mapS3Error maps NoSuchKey/NoSuchBucket and their smithy API-error-code
// equivalents to storage.ErrNotFound, and access-denied-shaped codes to
// storage.ErrAccessDenied, wrapping the original error in both cases so
// errors.Is still finds it.
func mapS3Error(err error, op string) error {
	var noSuchKey *types.NoSuchKey
	var noSuchBucket *types.NoSuchBucket
	if errors.As(err, &noSuchKey) || errors.As(err, &noSuchBucket) {
		return fmt.Errorf("storage/s3: %s: %w", op, storage.ErrNotFound)
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NoSuchBucket", "NotFound":
			return fmt.Errorf("storage/s3: %s: %w", op, storage.ErrNotFound)
		case "AccessDenied", "Forbidden", "AllAccessDisabled":
			return fmt.Errorf("storage/s3: %s: %w", op, storage.ErrAccessDenied)
		}
	}
	return fmt.Errorf("storage/s3: %s: %w", op, err)
}

// DefaultEncryptionConfirmed reads the bucket's default encryption
// (GetBucketEncryption; AWS S3 and RustFS implement it). Any server-side rule
// (SSE-S3, SSE-KMS) confirms it. No configuration
// (ServerSideEncryptionConfigurationNotFoundError) is a refusal. A backend
// that does not implement the call answers storage.ErrEncryptionUnreportable,
// which the operator may attest (ADR-0034 decision 4).
func (b *Backend) DefaultEncryptionConfirmed(ctx context.Context) (bool, error) {
	output, err := b.client.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: aws.String(b.bucket)})
	if err != nil {
		return encryptionAnswer(err)
	}
	if output.ServerSideEncryptionConfiguration == nil {
		return false, nil
	}
	for _, rule := range output.ServerSideEncryptionConfiguration.Rules {
		if rule.ApplyServerSideEncryptionByDefault != nil && rule.ApplyServerSideEncryptionByDefault.SSEAlgorithm != "" {
			return true, nil
		}
	}
	return false, nil
}

// fullObjectSHA256 answers the raw digest of a full-object SHA-256 checksum.
// A multipart upload's composite value ("<b64>-<parts>") is not the digest of
// the bytes, so it answers nil, and so does an absent or malformed value.
func fullObjectSHA256(value *string, kind types.ChecksumType) []byte {
	if value == nil || kind == types.ChecksumTypeComposite {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(*value)
	if err != nil || len(raw) != sha256.Size {
		return nil
	}
	return raw
}

// encryptionAnswer maps a GetBucketEncryption error: no configuration is "not
// encrypted", an unimplemented call is "cannot report", and anything else is
// an error (the check runs again at the next start).
func encryptionAnswer(err error) (bool, error) {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "ServerSideEncryptionConfigurationNotFoundError":
			return false, nil
		case "NotImplemented", "MethodNotAllowed", "NotSupported", "UnsupportedOperation":
			return false, fmt.Errorf("storage/s3: read bucket default encryption: %w: %w", storage.ErrEncryptionUnreportable, err)
		}
	}
	var status interface{ HTTPStatusCode() int }
	if errors.As(err, &status) && (status.HTTPStatusCode() == 501 || status.HTTPStatusCode() == 405) {
		return false, fmt.Errorf("storage/s3: read bucket default encryption: %w: %w", storage.ErrEncryptionUnreportable, err)
	}
	return false, fmt.Errorf("storage/s3: read bucket default encryption: %w", err)
}

var _ storage.ObjectCopier = (*Backend)(nil)

// Copy is S3 CopyObject inside the backend's bucket. It asks for a SHA-256
// checksum on the copy, so a copied segment still reports its full-object
// digest at HEAD (a single CopyObject is limited to 5 GiB, the single-PUT
// ceiling a checksum-bound segment already respects).
func (b *Backend) Copy(ctx context.Context, src, dst storage.ObjectRef) (storage.ObjectInfo, error) {
	source := b.bucket + "/" + src.StorageKey(b.keyPrefix)
	out, err := b.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:            aws.String(b.bucket),
		Key:               aws.String(dst.StorageKey(b.keyPrefix)),
		CopySource:        aws.String((&url.URL{Path: source}).EscapedPath()),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
	})
	if err != nil {
		return storage.ObjectInfo{}, mapS3Error(err, "copy object")
	}
	info := storage.ObjectInfo{Key: dst.Key()}
	if out.CopyObjectResult != nil {
		info.ETag = strings.Trim(aws.ToString(out.CopyObjectResult.ETag), `"`)
		info.LastModified = aws.ToTime(out.CopyObjectResult.LastModified)
		info.DigestSHA256 = fullObjectSHA256(out.CopyObjectResult.ChecksumSHA256, out.CopyObjectResult.ChecksumType)
	}
	return info, nil
}
