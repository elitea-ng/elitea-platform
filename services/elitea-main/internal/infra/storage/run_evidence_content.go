package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runevidence"
)

// RunEvidenceContent is runevidence.ContentStore over the object store: step
// content (ADR-0042 decision 2) in the PLATFORM-scoped bucket
// runevidence.ContentBucket, keyed <project>/runs/<run>/<step>/<side>. The
// platform scope keeps it out of every project's artifact routes.
type RunEvidenceContent struct {
	store ObjectStore
}

var _ runevidence.ContentStore = RunEvidenceContent{}

// NewRunEvidenceContent wraps store; a nil store is refused.
func NewRunEvidenceContent(store ObjectStore) (RunEvidenceContent, error) {
	if store == nil {
		return RunEvidenceContent{}, errors.New("run evidence content needs an object store")
	}
	return RunEvidenceContent{store: store}, nil
}

// Put stores one side. The backend verifies digest where it can.
func (c RunEvidenceContent) Put(ctx context.Context, key string, body []byte, mediaType string, digest []byte) error {
	ref, err := NewPlatformObjectRef(runevidence.ContentBucket, key)
	if err != nil {
		return err
	}
	_, err = c.store.Put(ctx, ref, bytes.NewReader(body), PutOptions{
		ContentType: mediaType, ContentLength: int64(len(body)), ChecksumSHA256: digest,
	})
	return err
}

// Get reads one side, at most runevidence.MaxContentBytes.
func (c RunEvidenceContent) Get(ctx context.Context, key string) ([]byte, error) {
	ref, err := NewPlatformObjectRef(runevidence.ContentBucket, key)
	if err != nil {
		return nil, err
	}
	body, _, err := c.store.Get(ctx, ref, nil)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, runevidence.MaxContentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > runevidence.MaxContentBytes {
		return nil, fmt.Errorf("step content object %s is larger than its bound", key)
	}
	return data, nil
}

// Delete removes objects; an absent object is not an error (Delete is
// idempotent on every backend).
func (c RunEvidenceContent) Delete(ctx context.Context, keys []string) error {
	var errs []error
	for _, key := range keys {
		ref, err := NewPlatformObjectRef(runevidence.ContentBucket, key)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := c.store.Delete(ctx, ref); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
