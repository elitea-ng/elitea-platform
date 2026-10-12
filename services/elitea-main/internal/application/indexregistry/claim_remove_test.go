package indexregistry

import (
	"context"
	"errors"
	"testing"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	indexmetaapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexmeta"
)

const (
	removeTestIndexID = "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b"
	removeTestOtherID = "1b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5c"
)

// removeStore is an in-memory registry Store with one live row.
type removeStore struct {
	row       *Row
	active    bool
	deleted   []string
	purged    []string
	findErr   error
	markCalls int
}

func (s *removeStore) List(context.Context, int32, int32) ([]Row, error) {
	if s.row == nil {
		return nil, nil
	}
	return []Row{*s.row}, nil
}

func (s *removeStore) FindByName(_ context.Context, projectID, toolkitID int32, name string) (Row, bool, error) {
	if s.findErr != nil {
		return Row{}, false, s.findErr
	}
	if s.row == nil || s.row.ProjectID != projectID || s.row.ToolkitID != toolkitID || s.row.Name != name {
		return Row{}, false, nil
	}
	return *s.row, true, nil
}

func (s *removeStore) SaveConfiguration(context.Context, int32, int32, string, []byte) error {
	return errors.New("not used")
}

func (s *removeStore) MarkDeleted(_ context.Context, projectID, toolkitID int32, indexID string) (Row, error) {
	s.markCalls++
	if s.row == nil || s.row.ProjectID != projectID || s.row.ToolkitID != toolkitID || s.row.IndexID != indexID {
		return Row{}, ErrNotFound
	}
	if s.active {
		return Row{}, ErrActiveRun
	}
	row := *s.row
	s.row = nil
	s.deleted = append(s.deleted, indexID)
	return row, nil
}

func (s *removeStore) PurgeDeleted(_ context.Context, indexID string) error {
	s.purged = append(s.purged, indexID)
	return nil
}

type removeToolkits struct {
	found bool
	calls []int32
}

func (t *removeToolkits) GetCurrentToolkit(_ context.Context, _ int32, actorID int32, toolkitID int32) (indexingapp.CurrentToolkitSnapshot, bool, error) {
	t.calls = append(t.calls, actorID)
	if !t.found {
		return indexingapp.CurrentToolkitSnapshot{}, false, nil
	}
	return indexingapp.CurrentToolkitSnapshot{ID: toolkitID, Type: "github", Name: "repo"}, true, nil
}

type removeSchedules struct {
	err   error
	calls []string
}

func (s *removeSchedules) DeleteCurrentIndexSchedule(_ context.Context, _, _ int32, name string) error {
	s.calls = append(s.calls, name)
	return s.err
}

type removeVectors struct {
	deleted []indexingapp.IndexVectorNamespace
}

func (v *removeVectors) DeleteIndexVectors(_ context.Context, ns indexingapp.IndexVectorNamespace) error {
	v.deleted = append(v.deleted, ns)
	return nil
}

func (v *removeVectors) Deferred() bool { return false }

type removeClaims struct {
	principal ClaimPrincipal
	err       error
	seen      []ClaimRef
}

func (c *removeClaims) AuthorizeClaim(_ context.Context, claim ClaimRef) (ClaimPrincipal, error) {
	c.seen = append(c.seen, claim)
	if c.err != nil {
		return ClaimPrincipal{}, c.err
	}
	return c.principal, nil
}

type removePermissions struct {
	allowed bool
	err     error
	asked   []string
}

func (p *removePermissions) HasProjectPermission(_ context.Context, userID, projectID int64, permission string) (bool, error) {
	p.asked = append(p.asked, permission)
	if userID != 11 || projectID != 7 {
		return false, nil
	}
	return p.allowed, p.err
}

type removeFixture struct {
	store       *removeStore
	toolkits    *removeToolkits
	schedules   *removeSchedules
	vectors     *removeVectors
	claims      *removeClaims
	permissions *removePermissions
	reported    []error
	remover     *ClaimRemover
}

func newRemoveFixture(t *testing.T) *removeFixture {
	t.Helper()
	f := &removeFixture{
		store: &removeStore{row: &Row{
			IndexID: removeTestIndexID, ProjectID: 7, ToolkitID: 19, Name: "docs", State: StateCompleted,
		}},
		toolkits:  &removeToolkits{found: true},
		schedules: &removeSchedules{},
		vectors:   &removeVectors{},
		claims: &removeClaims{principal: ClaimPrincipal{
			ProjectID: 7, ActorID: "11", Initiator: "user",
		}},
		permissions: &removePermissions{allowed: true},
	}
	service, err := NewService(f.toolkits, f.store, f.schedules, f.vectors, func(err error) { f.reported = append(f.reported, err) })
	if err != nil {
		t.Fatal(err)
	}
	f.remover, err = NewClaimRemover(f.claims, f.permissions, service, func(err error) { f.reported = append(f.reported, err) })
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func validClaim() ClaimRef {
	return ClaimRef{
		ExecutionID: "execution-1", Generation: 1, ClaimID: "claim-1",
		FenceToken: []byte("fence"), WorkloadIdentity: "spiffe://elitea.test/workload/worker-1",
	}
}

func validRemoval() ClaimRemovalRequest {
	return ClaimRemovalRequest{ToolkitID: 19, IndexName: "docs", IndexID: removeTestIndexID}
}

// The happy path: the claim resolves to the run's user, that user holds the
// delete permission in the claim's project, the named index is the frozen one,
// and the registry's own delete flow runs — tombstone, vectors, schedule —
// as that user. The project is the claim's; the request never names one.
func TestClaimRemoverRunsTheDeleteFlowAsTheRunsPrincipal(t *testing.T) {
	f := newRemoveFixture(t)
	disposition, err := f.remover.RemoveIndex(context.Background(), validClaim(), validRemoval())
	if err != nil {
		t.Fatal(err)
	}
	if disposition != RemovalRemoved {
		t.Fatalf("disposition = %q", disposition)
	}
	if len(f.claims.seen) != 1 || f.claims.seen[0].ClaimID != "claim-1" {
		t.Fatalf("claims seen = %+v", f.claims.seen)
	}
	if len(f.permissions.asked) != 1 || f.permissions.asked[0] != IndexDeletePermission {
		t.Fatalf("permissions asked = %v", f.permissions.asked)
	}
	// The toolkit is read as the run's user (Find and Delete each check it).
	for _, actor := range f.toolkits.calls {
		if actor != 11 {
			t.Fatalf("toolkit read as actor %d, not the run's user", actor)
		}
	}
	if len(f.store.deleted) != 1 || f.store.deleted[0] != removeTestIndexID {
		t.Fatalf("tombstoned = %v", f.store.deleted)
	}
	if len(f.vectors.deleted) != 1 || f.vectors.deleted[0] != (indexingapp.IndexVectorNamespace{ProjectID: 7, IndexID: removeTestIndexID}) {
		t.Fatalf("vectors deleted = %+v", f.vectors.deleted)
	}
	if len(f.store.purged) != 1 || len(f.schedules.calls) != 1 || f.schedules.calls[0] != "docs" {
		t.Fatalf("purged = %v schedules = %v", f.store.purged, f.schedules.calls)
	}
	if len(f.reported) != 0 {
		t.Fatalf("reported = %v", f.reported)
	}
}

// Everything that stops the delete before the registry is touched.
func TestClaimRemoverRefusesWithoutAnAuthorizedUserPrincipalHoldingThePermission(t *testing.T) {
	cases := map[string]struct {
		arrange     func(*removeFixture)
		disposition RemovalDisposition
		err         error
	}{
		"unauthorized claim": {
			arrange: func(f *removeFixture) { f.claims.err = ErrClaimUnauthorized },
			err:     ErrClaimUnauthorized,
		},
		"authorizer unavailable": {
			arrange: func(f *removeFixture) { f.claims.err = errors.New("db down") },
			err:     ErrClaimRemovalUnavailable,
		},
		"schedule-initiated run has no user": {
			arrange: func(f *removeFixture) {
				f.claims.principal = ClaimPrincipal{ProjectID: 7, ActorID: "11", Initiator: "schedule"}
			},
			disposition: RemovalForbidden,
		},
		"system actor is not a user": {
			arrange: func(f *removeFixture) {
				f.claims.principal = ClaimPrincipal{ProjectID: 7, ActorID: "system:index", Initiator: "user"}
			},
			disposition: RemovalForbidden,
		},
		"user without the permission": {
			arrange:     func(f *removeFixture) { f.permissions.allowed = false },
			disposition: RemovalForbidden,
		},
		"permission resolver unavailable": {
			arrange: func(f *removeFixture) { f.permissions.err = errors.New("rbac down") },
			err:     ErrClaimRemovalUnavailable,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRemoveFixture(t)
			tc.arrange(f)
			disposition, err := f.remover.RemoveIndex(context.Background(), validClaim(), validRemoval())
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v; expected %v", err, tc.err)
				}
			} else if err != nil || disposition != tc.disposition {
				t.Fatalf("disposition = %q err = %v; expected %q", disposition, err, tc.disposition)
			}
			if f.store.markCalls != 0 || len(f.vectors.deleted) != 0 {
				t.Fatal("the registry was touched before the principal was admitted")
			}
		})
	}
}

// The index must be the frozen one: an unknown name, a name that now resolves
// to a recreated index (another id), or a toolkit the principal cannot see is
// NotFound, and nothing is deleted. An active run is reported as such.
func TestClaimRemoverNamesTheFrozenIndexAndReportsTheRegistrysAnswer(t *testing.T) {
	cases := map[string]struct {
		arrange     func(*removeFixture) ClaimRemovalRequest
		disposition RemovalDisposition
		deleted     bool
	}{
		"unknown name": {
			arrange: func(*removeFixture) ClaimRemovalRequest {
				r := validRemoval()
				r.IndexName = "nope"
				return r
			},
			disposition: RemovalNotFound,
		},
		"recreated under the same name": {
			arrange: func(*removeFixture) ClaimRemovalRequest {
				r := validRemoval()
				r.IndexID = removeTestOtherID
				return r
			},
			disposition: RemovalNotFound,
		},
		"no frozen id still deletes by name": {
			arrange: func(*removeFixture) ClaimRemovalRequest {
				r := validRemoval()
				r.IndexID = ""
				return r
			},
			disposition: RemovalRemoved,
			deleted:     true,
		},
		"toolkit the principal cannot see": {
			arrange: func(f *removeFixture) ClaimRemovalRequest {
				f.toolkits.found = false
				return validRemoval()
			},
			disposition: RemovalNotFound,
		},
		"active run": {
			arrange: func(f *removeFixture) ClaimRemovalRequest {
				f.store.active = true
				return validRemoval()
			},
			disposition: RemovalActiveRun,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRemoveFixture(t)
			request := tc.arrange(f)
			disposition, err := f.remover.RemoveIndex(context.Background(), validClaim(), request)
			if err != nil {
				t.Fatal(err)
			}
			if disposition != tc.disposition {
				t.Fatalf("disposition = %q; expected %q", disposition, tc.disposition)
			}
			if (len(f.store.deleted) == 1) != tc.deleted {
				t.Fatalf("deleted = %v; expected deleted=%v", f.store.deleted, tc.deleted)
			}
		})
	}
}

// A schedule cleanup that fails AFTER the tombstone is reported, not surfaced:
// the index is gone for the model. A registry that cannot answer is retryable.
func TestClaimRemoverReportsLateCleanupAndSurfacesUnavailability(t *testing.T) {
	f := newRemoveFixture(t)
	f.schedules.err = indexmetaapp.ErrCurrentIndexScheduleToolkitMissing
	disposition, err := f.remover.RemoveIndex(context.Background(), validClaim(), validRemoval())
	if err != nil || disposition != RemovalRemoved {
		t.Fatalf("disposition = %q err = %v", disposition, err)
	}
	if len(f.store.deleted) != 1 || len(f.reported) != 1 {
		t.Fatalf("deleted = %v reported = %v", f.store.deleted, f.reported)
	}

	f = newRemoveFixture(t)
	f.store.findErr = errors.New("registry down")
	_, err = f.remover.RemoveIndex(context.Background(), validClaim(), validRemoval())
	if !errors.Is(err, ErrClaimRemovalUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

// Bounds: a malformed request or an incomplete claim never reaches the
// authorizer.
func TestClaimRemoverRefusesMalformedRequestsAndIncompleteClaims(t *testing.T) {
	f := newRemoveFixture(t)
	for name, request := range map[string]ClaimRemovalRequest{
		"no toolkit":   {IndexName: "docs"},
		"no name":      {ToolkitID: 19},
		"bad id shape": {ToolkitID: 19, IndexName: "docs", IndexID: "not-a-uuid"},
	} {
		if _, err := f.remover.RemoveIndex(context.Background(), validClaim(), request); !errors.Is(err, ErrInvalidClaimRemovalRequest) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	claim := validClaim()
	claim.WorkloadIdentity = ""
	if _, err := f.remover.RemoveIndex(context.Background(), claim, validRemoval()); !errors.Is(err, ErrClaimUnauthorized) {
		t.Fatalf("err = %v", err)
	}
	if len(f.claims.seen) != 0 {
		t.Fatal("the authorizer was asked about a request that was refused up front")
	}
}
