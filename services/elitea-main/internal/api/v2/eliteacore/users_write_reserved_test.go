package eliteacore

// The reserved break-glass account (ADR-0059 R2.6b, issue 2599) joins no
// project with a role: inviting its address, or giving a role to it, is
// refused before the caller is resolved. Removing roles is not refused (a
// cleanup must always be possible).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

type reservedRow struct {
	reserved bool
	err      error
}

func (r reservedRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*bool) = r.reserved
	return nil
}

// reservedQuerier answers the one reserved-account read.
type reservedQuerier struct {
	row   reservedRow
	reads int
}

func (q *reservedQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected query")
}

func (q *reservedQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	q.reads++
	return q.row
}

func TestMembershipWritesRefuseTheReservedAccount(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	for name, tc := range map[string]struct {
		change    membershipChange
		row       reservedRow
		wantCode  string
		wantReads int
	}{
		"invite the address": {
			change:   membershipChange{assigned: []int{3}, invited: []string{"a@corp.com", " Break-Glass@centry.user"}},
			wantCode: adminaccess.CodeReservedAccount,
		},
		"give it a role": {
			change: membershipChange{assigned: []int{3}, targets: []int{4, 9}}, row: reservedRow{reserved: true},
			wantCode: adminaccess.CodeReservedAccount, wantReads: 1,
		},
		"the read fails": {
			change: membershipChange{assigned: []int{3}, targets: []int{4}}, row: reservedRow{err: errors.New("down")},
			wantCode: "unresolved", wantReads: 1,
		},
		"an ordinary member": {
			change: membershipChange{assigned: []int{3}, targets: []int{4}}, wantReads: 1,
		},
		"remove its roles": {change: membershipChange{targets: []int{9}}, row: reservedRow{reserved: true}},
	} {
		t.Run(name, func(t *testing.T) {
			q := &reservedQuerier{row: tc.row}
			refusal := reservedMembershipRefusal(request, q, tc.change)
			if q.reads != tc.wantReads {
				t.Fatalf("%d reads, want %d", q.reads, tc.wantReads)
			}
			switch {
			case tc.wantCode == "" && refusal != nil:
				t.Fatalf("refused: %+v", refusal)
			case tc.wantCode != "" && refusal == nil:
				t.Fatalf("not refused, want %s", tc.wantCode)
			case tc.wantCode == adminaccess.CodeReservedAccount && refusal.Code != tc.wantCode:
				t.Fatalf("refusal code %q, want %q", refusal.Code, tc.wantCode)
			}
		})
	}
}
