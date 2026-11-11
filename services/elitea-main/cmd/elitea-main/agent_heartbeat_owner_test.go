package main

import (
	"context"
	"errors"
	"testing"

	v2pipelinetriggers "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/pipelinetriggers"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type heartbeatOwnerResolver struct {
	permissions []string
	err         error
}

func (r heartbeatOwnerResolver) ResolvePermissions(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{UserID: 3, Permissions: r.permissions}, r.err
}

// The heartbeat reads the owner's run permission through the pipeline's
// run-identity check (#1868), and keeps a resolver error an error.
func TestOwnerCanRunUsesThePipelineRunIdentityCheck(t *testing.T) {
	ctx := context.Background()
	if ok, err := ownerCanRun(heartbeatOwnerResolver{permissions: []string{v2pipelinetriggers.RunPermission}})(ctx, 1, 3); err != nil || !ok {
		t.Fatalf("held = %v, %v", ok, err)
	}
	if ok, err := ownerCanRun(heartbeatOwnerResolver{})(ctx, 1, 3); err != nil || ok {
		t.Fatalf("not held = %v, %v", ok, err)
	}
	boom := errors.New("rbac down")
	if ok, err := ownerCanRun(heartbeatOwnerResolver{err: boom})(ctx, 1, 3); !errors.Is(err, boom) || ok {
		t.Fatalf("resolver error = %v, %v", ok, err)
	}
	// No resolver is no decision: the act fails, it does not read as a lost
	// permission.
	if ok, err := ownerCanRun(nil)(ctx, 1, 3); !errors.Is(err, v2pipelinetriggers.ErrNoRunPermissionResolver) || ok {
		t.Fatalf("no resolver = %v, %v", ok, err)
	}
}
