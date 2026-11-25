package adminaccess_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

func TestNoticeRetryDelayDoublesUpToAnHour(t *testing.T) {
	for attempt, want := range map[int]time.Duration{
		0: time.Minute, 1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute,
		7: 60 * time.Minute, 8: time.Hour, 1000: time.Hour,
	} {
		if got := adminaccess.NoticeRetryDelay(attempt); got != want {
			t.Errorf("NoticeRetryDelay(%d) = %s, want %s", attempt, got, want)
		}
	}
}

// An `inherit` token ends at the later of the sunset date and 30 days after
// its owner's notice was delivered; nothing while the date is not set; the
// sunset date before any delivery (ADR-0059 decision 2).
func TestInheritTokenEndsOn(t *testing.T) {
	delivered := time.Date(2027, 1, 10, 22, 0, 0, 0, time.FixedZone("EST", -5*3600))
	for name, test := range map[string]struct {
		sunset    string
		delivered time.Time
		want      string
	}{
		"no sunset date":              {"", delivered, ""},
		"not delivered":               {"2027-01-31", time.Time{}, "2027-01-31"},
		"delivery plus 30 days later": {"2027-01-31", delivered, "2027-02-10"},
		"sunset date later":           {"2027-06-01", delivered, "2027-06-01"},
	} {
		if got := adminaccess.InheritTokenEndsOn(test.sunset, test.delivered); got != test.want {
			t.Errorf("%s: got %q, want %q", name, got, test.want)
		}
	}
}

func TestNoticeNoApprover(t *testing.T) {
	for path, want := range map[string]bool{
		adminaccess.ElevationPathSelf: true, adminaccess.ElevationPathSealed: true, adminaccess.ElevationPathApproved: false,
	} {
		if got := (adminaccess.Notice{Path: path}).NoApprover(); got != want {
			t.Errorf("%s: NoApprover = %v", path, got)
		}
	}
}

func TestElevationApprovalPath(t *testing.T) {
	id := uuid.MustParse("0b5a1c8e-2f41-4c55-9a7e-2d1f3e4a5b6c")
	if got := adminaccess.ElevationApprovalPath(id); got != "/admin/app/break-glass/requests/"+id.String() {
		t.Fatalf("path = %q", got)
	}
}

// refusingExecer fails the test when a refusal reaches the database.
type refusingExecer struct{ t *testing.T }

func (r refusingExecer) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	r.t.Error("a refused notice reached the database")
	return pgconn.CommandTag{}, nil
}

func TestQueueInheritTokenNoticeRefusesBadInput(t *testing.T) {
	owner := adminaccess.InheritTokenOwner{UserID: 1, Email: "a@example.com", Tokens: []adminaccess.NoticeToken{{ID: 1}}}
	at := time.Unix(1, 0)
	for name, test := range map[string]struct {
		owner  adminaccess.InheritTokenOwner
		sunset string
		at     time.Time
	}{
		"no address":   {adminaccess.InheritTokenOwner{UserID: 1, Tokens: owner.Tokens}, "2027-01-01", at},
		"no token":     {adminaccess.InheritTokenOwner{UserID: 1, Email: "a@example.com"}, "2027-01-01", at},
		"no sunset":    {owner, "", at},
		"no time":      {owner, "2027-01-01", time.Time{}},
		"no person id": {adminaccess.InheritTokenOwner{Email: "a@example.com", Tokens: owner.Tokens}, "2027-01-01", at},
	} {
		if _, err := adminaccess.QueueInheritTokenNotice(context.Background(), refusingExecer{t}, test.owner, test.sunset, test.at); err == nil {
			t.Errorf("%s: queued", name)
		}
	}
}
