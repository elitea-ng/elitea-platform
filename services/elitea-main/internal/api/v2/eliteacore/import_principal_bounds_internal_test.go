package eliteacore

import (
	"context"
	"math"
	"strconv"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/ownership"
)

// importPrincipalUserID narrows an int64 owner id to `int`. `int` is 32 bits on
// a 32-bit build, so an id above math.MaxInt32 truncates into an id that
// belongs to a different account — the same mis-attribution the function was
// written to stop (#504, CodeQL go/incorrect-integer-conversion). The author
// column is INTEGER, so an id outside that range names no row at all. Out of
// range is refused, not truncated.
func TestImportPrincipalUserIDRefusesAnIDTheAuthorColumnCannotHold(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name   string
		userID string
		wantID ownership.UserID
		wantOK bool
	}{
		{name: "an ordinary id", userID: "42", wantID: 42, wantOK: true},
		{name: "the highest id the column holds", userID: strconv.FormatInt(math.MaxInt32, 10), wantID: math.MaxInt32, wantOK: true},
		{name: "one above the column", userID: strconv.FormatInt(math.MaxInt32+1, 10), wantID: 0, wantOK: false},
		{name: "the highest id ParseInt accepts", userID: strconv.FormatInt(math.MaxInt64, 10), wantID: 0, wantOK: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx := auth.ContextWithUser(context.Background(), auth.User{UserID: testCase.userID})
			id, ok := importPrincipalUserID(ctx)
			if id != testCase.wantID || ok != testCase.wantOK {
				t.Fatalf("importPrincipalUserID with user id %q = (%d, %t), want (%d, %t)",
					testCase.userID, id, ok, testCase.wantID, testCase.wantOK)
			}
		})
	}
}
