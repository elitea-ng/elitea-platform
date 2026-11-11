package pipelinetriggers

import (
	"context"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type recordedResolver struct {
	permissions []string
	err         error
	mode        string
	user        auth.User
	project     string
}

func (r *recordedResolver) ResolvePermissions(_ context.Context, principal auth.User, mode, projectID string) (auth.PermissionResolution, error) {
	r.mode, r.user, r.project = mode, principal, projectID
	return auth.PermissionResolution{UserID: 7, Permissions: r.permissions}, r.err
}

func TestHoldsRunPermissionIsTheOneRunIdentityCheck(t *testing.T) {
	ctx := context.Background()

	held := &recordedResolver{permissions: []string{"other", RunPermission}}
	if ok, err := HoldsRunPermission(ctx, held, 12, 7); err != nil || !ok {
		t.Fatalf("a held run permission = %v, %v", ok, err)
	}
	if held.mode != RunPermissionMode || held.project != "12" || held.user.ID != "7" || held.user.UserID != "7" {
		t.Fatalf("resolved as %+v", held)
	}

	missing := &recordedResolver{permissions: []string{"other"}}
	if ok, err := HoldsRunPermission(ctx, missing, 12, 7); err != nil || ok {
		t.Fatalf("a missing run permission = %v, %v", ok, err)
	}

	boom := errors.New("rbac down")
	if ok, err := HoldsRunPermission(ctx, &recordedResolver{err: boom}, 12, 7); !errors.Is(err, boom) || ok {
		t.Fatalf("a resolver error = %v, %v", ok, err)
	}

	if ok, err := HoldsRunPermission(ctx, nil, 12, 7); err != nil || ok {
		t.Fatalf("a nil resolver = %v, %v", ok, err)
	}

	// The trigger path keeps failing closed on both a refusal and an error.
	for _, resolver := range []auth.PermissionResolver{missing, &recordedResolver{err: boom}, nil} {
		h := &Handler{permissions: resolver}
		if err := h.authorizeRunIdentity(ctx, 12, 7); !errors.Is(err, ErrRunForbidden) {
			t.Fatalf("authorizeRunIdentity(%T) = %v", resolver, err)
		}
	}
	if err := (&Handler{permissions: held}).authorizeRunIdentity(ctx, 12, 7); err != nil {
		t.Fatalf("authorizeRunIdentity with the permission = %v", err)
	}
}
