package codeplatform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
)

type Journal interface {
	Lookup(context.Context, domain.Admission, uint64) (domain.Record, bool, error)
	Begin(context.Context, domain.Admission, domain.Intent) (domain.Record, error)
	Dispatch(context.Context, domain.Admission, domain.Record) (bool, error)
	Commit(context.Context, domain.Admission, domain.Record) error
	domain.SigningAuthority
}
type PrivateContent interface {
	Put(context.Context, domain.ContentBinding, string, []byte) (string, error)
	Read(context.Context, domain.ContentBinding, string, string) ([]byte, error)
}
type ReplySigner interface {
	SignCommittedCodeCall(context.Context, domain.SigningAuthority, domain.Admission, domain.Record) (domain.SignedReply, error)
}

// Outcome is supplied by the concrete native operation owner. Known means the
// original operation's outcome is established; a timeout cannot set it true.
type Outcome struct {
	Status       string
	Result       json.RawMessage
	Payload      []byte
	OwnerReceipt string
	Known        bool
}
type Operations interface {
	// AuthorizeReply performs current read-only resource checks. It must never
	// repeat the original effect or redeem a caller-supplied grant.
	AuthorizeReply(context.Context, domain.Admission, domain.Record, domain.Request, domain.Reply) error
	Execute(context.Context, domain.Admission, domain.Record, domain.Request) (Outcome, error)
	// Reconcile can observe the original operation only. It cannot submit work.
	Reconcile(context.Context, domain.Admission, domain.Record, domain.Request) (Outcome, bool, error)
}
type ToolkitReconciliationJournal interface {
	CommitCodeToolkitReconciliation(context.Context, domain.Admission, domain.Record) error
}

// Service accepts no actor, project, runtime, or grant from the CP1 frame.
// Its admission is constructed by Main's original-visit/retained-runtime owner.
type Service struct {
	journal    Journal
	content    PrivateContent
	operations Operations
	signer     ReplySigner
}

func NewService(journal Journal, content PrivateContent, operations Operations, signer ReplySigner) (*Service, error) {
	if journal == nil || content == nil || operations == nil || signer == nil {
		return nil, domain.ErrUnavailable
	}
	return &Service{journal: journal, content: content, operations: operations, signer: signer}, nil
}

// Call processes one exact retained-process frame. It never retries a dispatch.
// A replay returns exact committed bytes, or reconciles an owning toolkit child.
func (s *Service) Call(ctx context.Context, a domain.Admission, frame []byte) ([]byte, error) {
	if s == nil || ctx == nil || a.Validate() != nil {
		return nil, domain.ErrUnauthorized
	}
	request, err := domain.DecodeRequest(frame)
	if err != nil || request.Sequence > a.Job.MaxCalls {
		return nil, domain.ErrFrame
	}
	saved, found, err := s.journal.Lookup(ctx, a, request.Sequence)
	if err != nil {
		return nil, err
	}
	if found {
		original, err := s.content.Read(ctx, domain.ContentForIntent(a.Job, saved.Intent), "intent", saved.Intent.EncryptedReference)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(original, frame) {
			return nil, domain.ErrConflict
		}
		intent, err := domain.NewIntent(a.Job, request, saved.Intent.EncryptedReference)
		if err != nil || !equalIntent(intent, saved.Intent) || saved.EffectID != domain.EffectID(a.Job, intent) {
			return nil, domain.ErrConflict
		}
		if saved.Committed() {
			return s.deliver(ctx, a, saved, request)
		}
	} else {
		contentBinding, err := domain.ContentForRequest(a.Job, request)
		if err != nil {
			return nil, err
		}
		reference, err := s.content.Put(ctx, contentBinding, "intent", frame)
		if err != nil {
			return nil, err
		}
		intent, err := domain.NewIntent(a.Job, request, reference)
		if err != nil {
			return nil, err
		}
		saved, err = s.journal.Begin(ctx, a, intent)
		if err != nil {
			return nil, err
		}
		// A competing caller may have committed the exact original sequence.
		if saved.Committed() {
			return s.deliver(ctx, a, saved, request)
		}
	}
	if saved.State == "dispatching" || saved.State == "uncertain" {
		if request.Operation != "toolkit_call" {
			return nil, domain.ErrUnknown
		}
		outcome, known, err := s.operations.Reconcile(ctx, a, saved, request)
		if err != nil {
			return nil, err
		}
		if !known || !outcome.Known {
			return nil, domain.ErrUnknown
		}
		owner, ok := s.journal.(ToolkitReconciliationJournal)
		if !ok {
			return nil, domain.ErrUnavailable
		}
		committed, err := s.response(ctx, a, saved, outcome)
		if err != nil {
			return nil, err
		}
		if err = owner.CommitCodeToolkitReconciliation(ctx, a, committed); err != nil {
			return nil, err
		}
		return s.deliver(ctx, a, committed, request)
	}
	if saved.State != "prepared" {
		return nil, domain.ErrConflict
	}
	dispatched, err := s.journal.Dispatch(ctx, a, saved)
	if err != nil {
		return nil, err
	}
	if !dispatched {
		return nil, domain.ErrUnknown
	}
	saved.State = "dispatching"
	outcome, err := s.operations.Execute(ctx, a, saved, request)
	if err != nil || !outcome.Known {
		// Keep dispatching durable. No fresh operation or Code run can follow this.
		return nil, domain.ErrUnknown
	}
	committed, err := s.response(ctx, a, saved, outcome)
	if err != nil {
		return nil, err
	}
	if err = s.journal.Commit(ctx, a, committed); err != nil {
		return nil, err
	}
	return s.deliver(ctx, a, committed, request)
}
func equalIntent(a, b domain.Intent) bool {
	return a.Sequence == b.Sequence && a.Operation == b.Operation && a.Call == b.Call && a.Resource == b.Resource && a.Arguments == b.Arguments && a.Payload == b.Payload && a.Frame == b.Frame && a.FrameBytes == b.FrameBytes
}
func (s *Service) response(ctx context.Context, a domain.Admission, saved domain.Record, outcome Outcome) (domain.Record, error) {
	if !outcome.Known || outcome.Status == "unknown_effect" || outcome.Status == "stopped" || outcome.Status == "lease_lost" {
		return domain.Record{}, domain.ErrUnknown
	}
	receipt := &domain.Receipt{EffectID: saved.EffectID, CallSHA256: hex.EncodeToString(saved.Intent.Call[:]), State: "committed"}
	exact, err := domain.EncodeReply(domain.Reply{Revision: 1, Sequence: saved.Intent.Sequence, Status: outcome.Status, Result: outcome.Result, Receipt: receipt}, outcome.Payload)
	if err != nil {
		return domain.Record{}, err
	}
	reference, err := s.content.Put(ctx, domain.ContentForIntent(a.Job, saved.Intent), "response", exact)
	if err != nil {
		return domain.Record{}, err
	}
	saved.State = "committed"
	saved.ResponseReference = reference
	saved.Response = sha256.Sum256(exact)
	saved.ResponseBytes = uint64(len(exact))
	saved.OwnerReceipt = outcome.OwnerReceipt
	if !saved.Committed() {
		return domain.Record{}, domain.ErrConflict
	}
	return saved, nil
}
func (s *Service) deliver(ctx context.Context, a domain.Admission, saved domain.Record, request domain.Request) ([]byte, error) {
	exact, err := s.content.Read(ctx, domain.ContentForIntent(a.Job, saved.Intent), "response", saved.ResponseReference)
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(exact) != saved.Response || uint64(len(exact)) != saved.ResponseBytes {
		return nil, domain.ErrConflict
	}
	reply, err := domain.DecodeCommittedReply(exact)
	if err != nil || reply.Sequence != saved.Intent.Sequence || reply.Receipt.EffectID != saved.EffectID || reply.Receipt.CallSHA256 != hex.EncodeToString(saved.Intent.Call[:]) {
		return nil, domain.ErrConflict
	}
	if err = s.operations.AuthorizeReply(ctx, a, saved, request, reply); err != nil {
		return nil, err
	}
	signed, err := s.signer.SignCommittedCodeCall(ctx, s.journal, a, saved)
	if err != nil {
		return nil, err
	}
	return domain.SignedMailboxFrame(signed, exact)
}
