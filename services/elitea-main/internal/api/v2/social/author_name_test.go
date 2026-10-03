package social_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// UI-UX-1(a): the chat greets the person by name, not by email. Without a
// database the principal's own name is the best answer the handler has.
func TestGetAuthorWithoutAPoolNamesThePrincipalBeforeTheEmail(t *testing.T) {
	for _, tc := range []struct {
		name string
		user auth.User
		want string
	}{
		{"named principal", auth.User{ID: "42", Email: "ada@example.com", Name: "Ada Lovelace"}, "Ada Lovelace"},
		{"blank name", auth.User{ID: "42", Email: "ada@example.com", Name: "   "}, "ada@example.com"},
		{"no name", auth.User{ID: "42", Email: "ada@example.com"}, "ada@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/author", nil)
			request = request.WithContext(auth.ContextWithUser(request.Context(), tc.user))
			recorder := httptest.NewRecorder()
			social.NewHandler(nil).GetAuthor(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d", recorder.Code)
			}
			var body struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Name != tc.want || body.Email != tc.user.Email {
				t.Fatalf("name = %q email = %q, want name %q", body.Name, body.Email, tc.want)
			}
		})
	}
}
