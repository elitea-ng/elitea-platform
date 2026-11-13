package apierr

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Issue 1968: a writer that skipped the handler check and hit a length CHECK
// answers the 400 a handler would have, not a 500.
func TestWriteMapsALengthCheckViolationTo400(t *testing.T) {
	err := fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23514", ConstraintName: "skills_description_max_len"})
	rec := httptest.NewRecorder()
	Write(rec, err)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var body Response
	if e := json.Unmarshal(rec.Body.Bytes(), &body); e != nil {
		t.Fatal(e)
	}
	if body.Field != "description" || body.MaxLength != 2000 {
		t.Fatalf("body = %+v", body)
	}
}

func TestWriteLeavesOtherCheckViolationsAs500(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, &pgconn.PgError{Code: "23514", ConstraintName: "something_else"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
