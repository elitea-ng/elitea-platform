package storage

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"io"
	"strconv"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
)

// CodePlatformContentKeys belongs to Main's configured key owner.
// Key material never enters Code, Worker commands, or shared streams.
type CodePlatformContentKeys interface {
	CurrentCodePlatformKey(context.Context) (string, [32]byte, error)
	ResolveCodePlatformKey(context.Context, string) ([32]byte, error)
}
type CodePlatformContent struct {
	store ObjectStore
	keys  CodePlatformContentKeys
}

func NewCodePlatformContent(store ObjectStore, keys CodePlatformContentKeys) (*CodePlatformContent, error) {
	if store == nil || keys == nil {
		return nil, domain.ErrUnavailable
	}
	return &CodePlatformContent{store: store, keys: keys}, nil
}
func codeContentAAD(binding domain.ContentBinding, kind, reference, keyID string) ([]byte, error) {
	if binding.Validate() != nil || (kind != "intent" && kind != "response") || !codeContentKeyID(keyID) {
		return nil, domain.ErrUnauthorized
	}
	if _, err := codeContentRef(binding.Job, reference); err != nil {
		return nil, err
	}
	job := binding.Job
	buffer := bytes.NewBufferString("elitea.code.platform-content.aes-gcm.v1\x00")
	for _, field := range []string{"elitea.code.platform-" + kind + ".v1", keyID, reference, job.TenantID, strconv.FormatInt(job.ProjectID, 10), job.ExecutionID, strconv.FormatUint(job.OriginalGeneration, 10), job.RuntimeID, strconv.FormatInt(job.ActorID, 10), strconv.FormatUint(binding.Sequence, 10), binding.Operation} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		buffer.Write(length[:])
		buffer.WriteString(field)
	}
	buffer.Write(job.Activation[:])
	buffer.Write(job.PreparedRequest[:])
	buffer.Write(job.Policy[:])
	buffer.Write(binding.Resource[:])
	buffer.Write(binding.Request[:])
	return buffer.Bytes(), nil
}
func codeContentBound(kind string) int {
	if kind == "intent" {
		return 8 + domain.MaxRequestHeader + domain.MaxChunk
	}
	return 8 + domain.MaxReplyHeader + domain.MaxChunk
}
func codeContentRef(job domain.Job, reference string) (ObjectRef, error) {
	if len(reference) != 68 || reference[64:] != ".cp1" {
		return ObjectRef{}, domain.ErrConflict
	}
	raw, err := hex.DecodeString(reference[:64])
	if err != nil || len(raw) != 32 || hex.EncodeToString(raw) != reference[:64] {
		return ObjectRef{}, domain.ErrConflict
	}
	// Platform scope cannot collide with a user project's artifact bucket.
	return NewPlatformObjectRef("code-platform-private", strconv.FormatInt(job.ProjectID, 10)+"/"+reference)
}

// Put binds a preassigned random reference into standard AES-GCM associated data.
// The journal binds the exact plaintext hash independently of the encrypted object.
func (s *CodePlatformContent) Put(ctx context.Context, binding domain.ContentBinding, kind string, data []byte) (string, error) {
	if s == nil || ctx == nil || binding.Validate() != nil || (kind != "intent" && kind != "response") || len(data) == 0 || len(data) > codeContentBound(kind) {
		return "", domain.ErrConflict
	}
	keyID, key, err := s.keys.CurrentCodePlatformKey(ctx)
	defer clearContentBytes(key[:])
	if err != nil {
		return "", domain.ErrUnavailable
	}
	var objectID [32]byte
	if _, err = rand.Read(objectID[:]); err != nil {
		return "", domain.ErrUnavailable
	}
	reference := hex.EncodeToString(objectID[:]) + ".cp1"
	aad, err := codeContentAAD(binding, kind, reference, keyID)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", domain.ErrUnavailable
	}
	gcm, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return "", domain.ErrUnavailable
	}
	sealed := []byte{'C', 'P', 2, byte(len(keyID))}
	sealed = append(sealed, []byte(keyID)...)
	sealed = gcm.Seal(sealed, nil, data, aad)
	ref, err := codeContentRef(binding.Job, reference)
	if err != nil {
		return "", err
	}
	_, err = s.store.Put(ctx, ref, bytes.NewReader(sealed), PutOptions{ContentType: "application/octet-stream", ContentLength: int64(len(sealed))})
	if err != nil {
		return "", domain.ErrUnavailable
	}
	return reference, nil
}

// Read checks the original scope, call, request, key and object reference before return.
func (s *CodePlatformContent) Read(ctx context.Context, binding domain.ContentBinding, kind, reference string) ([]byte, error) {
	if s == nil || ctx == nil || binding.Validate() != nil || (kind != "intent" && kind != "response") {
		return nil, domain.ErrConflict
	}
	ref, err := codeContentRef(binding.Job, reference)
	if err != nil {
		return nil, err
	}
	reader, info, err := s.store.Get(ctx, ref, nil)
	if err != nil {
		return nil, domain.ErrUnavailable
	}
	maximum := codeContentBound(kind) + 4 + 64 + 12 + 16
	if info.Size < 1 || info.Size > int64(maximum) {
		if reader.Close() != nil {
			return nil, domain.ErrUnavailable
		}
		return nil, domain.ErrConflict
	}
	sealed, readErr := io.ReadAll(io.LimitReader(reader, int64(maximum)+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || len(sealed) > maximum || int64(len(sealed)) != info.Size || len(sealed) < 4+1+12+16 || sealed[0] != 'C' || sealed[1] != 'P' || sealed[2] != 2 {
		return nil, domain.ErrConflict
	}
	keyBytes := int(sealed[3])
	if keyBytes < 1 || keyBytes > 64 || len(sealed) < 4+keyBytes+12+16 {
		return nil, domain.ErrConflict
	}
	keyID := string(sealed[4 : 4+keyBytes])
	aad, err := codeContentAAD(binding, kind, reference, keyID)
	if err != nil {
		return nil, err
	}
	key, err := s.keys.ResolveCodePlatformKey(ctx, keyID)
	defer clearContentBytes(key[:])
	if err != nil {
		return nil, domain.ErrUnavailable
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, domain.ErrUnavailable
	}
	gcm, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, domain.ErrUnavailable
	}
	data, err := gcm.Open(nil, nil, sealed[4+keyBytes:], aad)
	if err != nil || len(data) > codeContentBound(kind) {
		clearContentBytes(data)
		return nil, domain.ErrConflict
	}
	return data, nil
}
