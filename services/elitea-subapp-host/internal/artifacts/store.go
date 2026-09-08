package artifacts

// The read and delete half of the artifact API, added for DeepWiki's
// wiki_query family (ADR-0022 parity): a wiki lives in the bucket as a set
// of keys under `{wiki_id}/`, so listing wikis and deleting one are bucket
// operations, not engine ones.
//
// THE ROUTES ARE THE MODERN ONES, for the reason
// apps/elitea-web/src/entities/wiki/api/wikiArtifactsApi.ts records: the
// legacy plugin spoke /artifacts/artifact(s)/default/{project}/… and
// elitea-main serves no route in that shape, so porting those paths would
// port a 404. These are /artifacts/objects/{project}/{bucket}[…], the ones
// the wiki browser already reads and deletes through.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Object is one listed object. Only the fields a caller here reads are
// decoded; the route answers more.
type Object struct {
	Key        string `json:"key"`
	SizeBytes  int64  `json:"size_bytes"`
	MediaType  string `json:"media_type"`
	ModifiedAt string `json:"modified_at"`
}

// Failure is one key a batch delete could not remove.
type Failure struct {
	Key     string `json:"key"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Store is the whole artifact surface: uploading, plus the reads and the
// batch delete a sub-application needs to manage what it produced. The
// upload-only Client stays the narrow interface a composer depends on; a
// caller that needs more asserts to this.
type Store interface {
	Client
	// List returns every object whose key starts with prefix, following the
	// route's cursor until it is exhausted.
	List(ctx context.Context, bucket, prefix string) ([]Object, error)
	// Download returns one object's bytes.
	Download(ctx context.Context, bucket, key string) ([]byte, error)
	// DeleteBatch removes keys in ONE request and reports what survived.
	// The failures are returned rather than folded into a count: a partial
	// delete leaves a half-removed wiki, and the operator has to be told
	// which keys remain.
	DeleteBatch(ctx context.Context, bucket string, keys []string) ([]string, []Failure, error)
}

const (
	// maxListPages bounds the cursor loop. A wiki bucket holds thousands of
	// keys, not millions; a listing that will not end is a server fault, and
	// looping forever on it would hang the invocation instead of failing it.
	maxListPages = 200
	// listPageSize is what one page asks for. The route's own cap is what
	// actually applies; this only keeps the number of round trips down.
	listPageSize = 1000
	// maxReadBytes bounds one read. A manifest or a registry is kilobytes;
	// anything of this size is not one, and reading it into memory to
	// discover that is the failure mode worth avoiding.
	maxReadBytes = 32 << 20
)

// objectPage is one listing answer, in both shapes it arrives in: bare, and
// wrapped in the transport's `data` envelope. Unwrapping is not optional —
// a reader typed as the bare body gets an empty list out of a perfectly
// good 200, which reads as an empty bucket (the #132 shape).
type objectPage struct {
	Objects    []Object `json:"objects"`
	NextCursor string   `json:"next_cursor"`
}

// List pages through the object listing.
func (c *HTTPClient) List(ctx context.Context, bucket, prefix string) ([]Object, error) {
	var objects []Object
	cursor := ""
	for page := 0; page < maxListPages; page++ {
		query := url.Values{}
		if prefix != "" {
			query.Set("prefix", prefix)
		}
		query.Set("limit", strconv.Itoa(listPageSize))
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var body struct {
			objectPage
			Data *objectPage `json:"data"`
		}
		if err := c.getJSON(ctx, c.ObjectsURL(bucket)+"?"+query.Encode(), &body); err != nil {
			return nil, err
		}
		listed, next := body.Objects, body.NextCursor
		if body.Data != nil && len(listed) == 0 {
			listed = body.Data.Objects
			if next == "" {
				next = body.Data.NextCursor
			}
		}
		objects = append(objects, listed...)
		if next == "" {
			return objects, nil
		}
		cursor = next
	}
	return nil, fmt.Errorf("listing %s did not end after %d pages", bucket, maxListPages)
}

// Download reads one object. The key is a PATH: each segment is escaped on
// its own, because escaping the whole key would turn its slashes into %2F
// and address an object that does not exist.
func (c *HTTPClient) Download(ctx context.Context, bucket, key string) ([]byte, error) {
	segments := strings.Split(key, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.ObjectsURL(bucket)+"/"+strings.Join(segments, "/"), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.Settings.APIKey)
	response, err := c.Client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("artifact not found: %s/%s", bucket, key)
	}
	if response.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return nil, fmt.Errorf("failed to download artifact: HTTP %d — %s",
			response.StatusCode, clip(string(text), 200))
	}
	return io.ReadAll(io.LimitReader(response.Body, maxReadBytes))
}

// deleteAnswer is the batch-delete response, in both shapes.
type deleteAnswer struct {
	Deleted []string  `json:"deleted"`
	Failed  []Failure `json:"failed"`
}

// DeleteBatch removes every key in one request.
func (c *HTTPClient) DeleteBatch(ctx context.Context, bucket string, keys []string) ([]string, []Failure, error) {
	if len(keys) == 0 {
		// Refused rather than sent, as the wiki browser's own delete does:
		// the route answers 400 for an empty batch, and a caller that sent
		// one would read that as a failed delete rather than as nothing to do.
		return nil, nil, fmt.Errorf("no keys were named for deletion")
	}
	body, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		return nil, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.ObjectsURL(bucket)+":batchDelete", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.Settings.APIKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.Client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	text, _ := io.ReadAll(io.LimitReader(response.Body, maxReadBytes))
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("failed to delete artifacts: HTTP %d — %s",
			response.StatusCode, clip(string(text), 500))
	}
	var decoded struct {
		deleteAnswer
		Data *deleteAnswer `json:"data"`
	}
	if err := json.Unmarshal(text, &decoded); err != nil {
		return nil, nil, fmt.Errorf("the delete response could not be read: %w", err)
	}
	deleted, failed := decoded.Deleted, decoded.Failed
	if decoded.Data != nil && len(deleted) == 0 && len(failed) == 0 {
		deleted, failed = decoded.Data.Deleted, decoded.Data.Failed
	}
	return deleted, failed, nil
}

// getJSON performs one bearer-authenticated GET and decodes the body.
func (c *HTTPClient) getJSON(ctx context.Context, target string, into any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.Settings.APIKey)
	response, err := c.Client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	text, _ := io.ReadAll(io.LimitReader(response.Body, maxReadBytes))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to list artifacts: HTTP %d — %s",
			response.StatusCode, clip(string(text), 500))
	}
	return json.Unmarshal(text, into)
}
