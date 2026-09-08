// Package ownership carries the two kinds of number that the `owner_id` and
// `author_id` columns hold.
//
// # Why two types and not two ints
//
// Issue #533: one column NAME holds a project in one tenant table and a user in
// the next. `p_<id>.applications.owner_id` and `p_<id>.skills.owner_id` hold the
// owning PROJECT; `p_<id>.chat_conversation_folders.owner_id` and every
// `author_id` hold the USER. Both are integers, so the compiler accepted a user
// id in a project column and the database accepted the row. The defect was
// visible only to a reader who joined the column and got rows that looked
// valid.
//
// ProjectID and UserID are distinct named types. Go does not convert between
// them without an explicit conversion, so a function that takes both — for
// example eliteacore.importSkill(ctx, schema, ProjectID, UserID, entry) — no
// longer accepts them in the wrong order. The mistake becomes a compile error
// at the call site instead of a wrong number in a row.
//
// The database half of the same rule is migrations/tenant/0131: a PROJECT-kind
// owner_id carries a FOREIGN KEY to centry.project(id), so a number that names
// no project is refused with SQLSTATE 23503.
//
// # Where each value comes from
//
// A ProjectID comes from the tenant schema the row lives in
// (internal/infra/db/tenantschema.OwnerID). A UserID comes from the
// authenticated principal. Neither is read out of a request body: an id from
// another installation names a local row that has nothing to do with it.
package ownership

import (
	"errors"
	"strconv"
)

// ErrNotAProjectID reports a value that cannot name a project.
var ErrNotAProjectID = errors.New("ownership: not a project id")

// ErrNotAUserID reports a value that cannot name a user.
var ErrNotAUserID = errors.New("ownership: not a user id")

// ProjectID is the owning PROJECT of a row. It is the value of a PROJECT-kind
// `owner_id` column.
type ProjectID int64

// UserID is the PRINCIPAL that created a row. It is the value of an `author_id`
// column, and of the USER-kind `owner_id` columns.
type UserID int64

// NewProjectID refuses a number that cannot name a project. Project ids come
// from centry.project's SERIAL, so the first one is 1.
func NewProjectID(value int64) (ProjectID, error) {
	if value <= 0 {
		return 0, ErrNotAProjectID
	}
	return ProjectID(value), nil
}

// NewUserID refuses a number that cannot name a user. User ids come from
// public.auth_core__user's SERIAL, so the first one is 1.
func NewUserID(value int64) (UserID, error) {
	if value <= 0 {
		return 0, ErrNotAUserID
	}
	return UserID(value), nil
}

// Int64 gives the number to a statement parameter or to a response field.
func (p ProjectID) Int64() int64 { return int64(p) }

// String gives the decimal form that the API responses carry.
func (p ProjectID) String() string { return strconv.FormatInt(int64(p), 10) }

// Int64 gives the number to a statement parameter or to a response field.
func (u UserID) Int64() int64 { return int64(u) }

// String gives the decimal form that the API responses carry.
func (u UserID) String() string { return strconv.FormatInt(int64(u), 10) }
