package identityrepo

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// The rows a Form user without an email used to get — `<login>@centry.user`
// with a provider link — are found, and the platform's own accounts are not.
// Nothing is changed by the read.
func TestListReservedDomainSignInAccountsFindsSynthesizedPeopleOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := newIdentityTestDatabase(t, ctx)

	empty, err := ListReservedDomainSignInAccounts(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Count != 0 || len(empty.ProviderReferences) != 0 {
		t.Fatalf("empty database reported %+v", empty)
	}

	mustExec(t, ctx, pool, `
INSERT INTO public.auth_core__user (id, email, name) VALUES
	(101, 'alice@centry.user', 'alice'),
	(102, 'system@centry.user', 'system'),
	(103, 'system_user_7@centry.user', 'project 7'),
	(104, 'bob@example.test', 'bob'),
	(105, 'Carol@CENTRY.user', 'carol'),
	(106, 'unlinked@centry.user', 'unlinked')`)
	mustExec(t, ctx, pool, `
INSERT INTO public.auth_core__user_provider (user_id, provider_ref) VALUES
	(101, 'alice'),
	(102, 'system-link'),
	(103, 'project-link'),
	(104, 'bob'),
	(105, 'carol')`)

	found, err := ListReservedDomainSignInAccounts(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if found.Count != 2 || !reflect.DeepEqual(found.ProviderReferences, []string{"alice", "carol"}) {
		t.Fatalf("found %+v, want alice and carol only", found)
	}
	// The read changed nothing: all five reserved-domain rows are still there.
	assertCount(t, ctx, pool, 5, `SELECT count(*) FROM public.auth_core__user WHERE email LIKE '%@centry.user' OR email LIKE '%@CENTRY.user'`)
}
