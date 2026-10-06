package codeplatform

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

var (
	ErrUnauthorized = errors.New("code platform authority refused")
	ErrUnavailable  = errors.New("code platform dependency unavailable")
	ErrUnknown      = errors.New("code platform effect requires reconciliation")
	ErrConflict     = errors.New("code platform immutable identity conflicts")
)

func jsonResource(request Request) ([]byte, error) { return json.Marshal(request.Resource) }

// Job binds one retained Code process. It contains no graph business state.
type Job struct {
	TenantID           string
	ExecutionID        string
	OriginalGeneration uint64
	Activation         [32]byte
	PreparedRequest    [32]byte
	Policy             [32]byte
	RuntimeID          string
	ProjectID          int64
	ActorID            int64
	MaxCalls           uint64
	MaxTotalBytes      uint64
}

func (j Job) Validate() error {
	if j.TenantID == "" || len(j.TenantID) > 256 || strings.ContainsAny(j.TenantID, "\x00\r\n") || j.ExecutionID == "" || len(j.ExecutionID) > 256 || strings.ContainsAny(j.ExecutionID, "\x00\r\n") || j.OriginalGeneration == 0 || j.Activation == [32]byte{} || j.PreparedRequest == [32]byte{} || j.Policy == [32]byte{} || j.RuntimeID == "" || len(j.RuntimeID) > 512 || strings.ContainsAny(j.RuntimeID, "\x00\r\n") || j.ProjectID <= 0 || j.ProjectID > 2147483647 || j.ActorID <= 0 || j.ActorID > 2147483647 || j.MaxCalls < 1 || j.MaxCalls > MaxCalls || j.MaxTotalBytes < 1 || j.MaxTotalBytes > 64*1024*1024 {
		return ErrUnauthorized
	}
	return nil
}

// Admission is populated by Main after current mTLS and saved job authorization.
// The browser and Code frame cannot provide these fields.
type Admission struct {
	Job              Job
	ClaimID          string
	Generation       uint64
	LeaseEpoch       uint64
	WorkloadIdentity string
	FenceToken       []byte
}

func (a Admission) Validate() error {
	if a.Job.Validate() != nil || a.Generation == 0 || a.LeaseEpoch == 0 || a.ClaimID == "" || len(a.ClaimID) > 256 || a.WorkloadIdentity == "" || len(a.WorkloadIdentity) > 1024 || len(a.FenceToken) < 1 || len(a.FenceToken) > 4096 {
		return ErrUnauthorized
	}
	return nil
}

type Intent struct {
	Sequence           uint64
	Operation          string
	Call               [32]byte
	Resource           [32]byte
	Arguments          [32]byte
	Payload            [32]byte
	Frame              [32]byte
	FrameBytes         uint64
	EncryptedReference string
}

func NewIntent(job Job, request Request, encryptedReference string) (Intent, error) {
	if job.Validate() != nil || request.Sequence == 0 || request.Sequence > job.MaxCalls || len(request.ExactFrame) == 0 || !validReference(encryptedReference) {
		return Intent{}, ErrConflict
	}
	resource, _ := jsonResource(request)
	return Intent{Sequence: request.Sequence, Operation: request.Operation, Call: request.CallDigest(job.Activation, job.PreparedRequest, job.Policy), Resource: sha256.Sum256(resource), Arguments: sha256.Sum256(request.Arguments), Payload: sha256.Sum256(request.Payload), Frame: sha256.Sum256(request.ExactFrame), FrameBytes: uint64(len(request.ExactFrame)), EncryptedReference: encryptedReference}, nil
}
func EffectID(job Job, intent Intent) string {
	hash := sha256.New()
	hash.Write([]byte("elitea.code.platform-effect.v1\x00"))
	for _, field := range []string{job.ExecutionID, strconv.FormatUint(job.OriginalGeneration, 10), job.RuntimeID} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		hash.Write(length[:])
		hash.Write([]byte(field))
	}
	hash.Write(job.Activation[:])
	hash.Write(job.PreparedRequest[:])
	hash.Write(job.Policy[:])
	hash.Write(intent.Call[:])
	return hex.EncodeToString(hash.Sum(nil))
}
func validReference(value string) bool {
	return len(value) == 68 && strings.HasSuffix(value, ".cp1") && digestPattern.MatchString(value[:64])
}

// Record owns one immutable operation and its observation state.
// Dispatching and uncertain records never grant a new dispatch.
type Record struct {
	Intent            Intent
	EffectID          string
	State             string
	ResponseReference string
	Response          [32]byte
	ResponseBytes     uint64
	OwnerReceipt      string
}

func (r Record) Committed() bool {
	return r.State == "committed" && validReference(r.ResponseReference) && r.Response != [32]byte{} && r.ResponseBytes > 0 && r.ResponseBytes <= 8+MaxReplyHeader+MaxChunk && len(r.OwnerReceipt) <= 1024
}

// ParentEffect is supplied only by the trusted Code broker.
// Native toolkit admission checks the persisted call before accepting this relation.
type ParentEffect struct {
	Admission Admission
	Intent    Intent
	EffectID  string
}

func (p ParentEffect) Validate() error {
	if p.Admission.Validate() != nil || p.Intent.Operation != "toolkit_call" || p.Intent.Sequence < 1 || p.Intent.Sequence > p.Admission.Job.MaxCalls || p.EffectID != EffectID(p.Admission.Job, p.Intent) {
		return ErrUnauthorized
	}
	return nil
}

// ToolkitChild is an original native execution-owner relation, never a caller selector.
type ToolkitChild struct {
	ExecutionID     string
	ToolkitID       int64
	Revision        string
	ArgumentsSHA256 [32]byte
	OwnerReceipt    string
	Settled         bool
}

// ContentBinding binds private bytes to one original call before encryption.
// It contains hashes, not secret values or caller-selected storage paths.
type ContentBinding struct {
	Job               Job
	Sequence          uint64
	Operation         string
	Resource, Request [32]byte
}

func ContentForRequest(job Job, request Request) (ContentBinding, error) {
	if job.Validate() != nil || request.Sequence < 1 || request.Sequence > job.MaxCalls || len(request.ExactFrame) == 0 {
		return ContentBinding{}, ErrConflict
	}
	resource, err := jsonResource(request)
	if err != nil {
		return ContentBinding{}, ErrConflict
	}
	return ContentBinding{Job: job, Sequence: request.Sequence, Operation: request.Operation, Resource: sha256.Sum256(resource), Request: sha256.Sum256(request.ExactFrame)}, nil
}
func ContentForIntent(job Job, intent Intent) ContentBinding {
	return ContentBinding{Job: job, Sequence: intent.Sequence, Operation: intent.Operation, Resource: intent.Resource, Request: intent.Frame}
}
func (b ContentBinding) Validate() error {
	if b.Job.Validate() != nil || b.Sequence < 1 || b.Sequence > b.Job.MaxCalls || b.Operation == "" || len(b.Operation) > 32 || b.Resource == [32]byte{} || b.Request == [32]byte{} {
		return ErrConflict
	}
	return nil
}
