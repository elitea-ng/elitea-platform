package scim

// What these tests hold in place.
//
// A SCIM client acts on the STATUS and the shape it gets back, and every
// dangerous failure on this surface is one where the shape is fine and the
// meaning is wrong:
//
//   - A filter that was ignored rather than refused: the client asked whether
//     an account exists and was answered about somebody else.
//   - A PATCH that answered 200 without applying its change: the identity
//     provider records the deactivation as done and never sends it again.
//   - A DELETE that answered 204 without revoking access.
//   - A numeric `id`: SCIM ids are strings, and a client that round-trips a
//     JSON number through a float loses large ones.
//
// The directory is a FAKE that records what it was asked to do, so each of
// these is asserted against the call that reached the store, not only against
// the response.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/scimdirectory"
)

type recordingDirectory struct {
	users       map[int]scimdirectory.User
	created     []scimdirectory.User
	replaced    []scimdirectory.User
	activeCalls []bool
	changes     []scimdirectory.UserChanges
	// conflictOnUserName makes ApplyUserChanges answer a uniqueness conflict.
	conflictOnUserName bool
	// protected makes every user write answer this refusal, as the store does
	// for an administrator's or a platform principal's row.
	protected    *scimdirectory.ProtectedError
	listedFilter scimdirectory.Filter
	// groups holds the /Groups half of the fake. It is a pointer to a type
	// declared in groups_internal_test.go so the group tests own their own
	// state, and a users test that never touches a group reads unchanged.
	groups *groupState
}

func newRecordingDirectory() *recordingDirectory {
	return &recordingDirectory{groups: newGroupState(), users: map[int]scimdirectory.User{
		42: {
			ID: 42, UserName: "alice@corp.com", DisplayName: "Alice", Active: true,
			ExternalID: "00u1abc", CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(2, 0),
		},
	}}
}

func (d *recordingDirectory) List(
	_ context.Context, filter scimdirectory.Filter, _, _ int,
) ([]scimdirectory.User, int, error) {
	d.listedFilter = filter
	users := make([]scimdirectory.User, 0, len(d.users))
	for _, user := range d.users {
		users = append(users, user)
	}
	return users, len(users), nil
}

func (d *recordingDirectory) Get(_ context.Context, id int) (scimdirectory.User, error) {
	user, ok := d.users[id]
	if !ok {
		return scimdirectory.User{}, scimdirectory.ErrNotFound
	}
	return user, nil
}

func (d *recordingDirectory) Create(
	_ context.Context, user scimdirectory.User,
) (scimdirectory.User, error) {
	d.created = append(d.created, user)
	user.ID = 43
	d.users[43] = user
	return user, nil
}

func (d *recordingDirectory) Replace(
	_ context.Context, id int, user scimdirectory.User,
) (scimdirectory.User, error) {
	if _, ok := d.users[id]; !ok {
		return scimdirectory.User{}, scimdirectory.ErrNotFound
	}
	if d.protected != nil {
		return scimdirectory.User{}, d.protected
	}
	user.ID = id
	d.replaced = append(d.replaced, user)
	d.users[id] = user
	return user, nil
}

func (d *recordingDirectory) SetActive(
	_ context.Context, id int, active bool,
) (scimdirectory.User, error) {
	user, ok := d.users[id]
	if !ok {
		return scimdirectory.User{}, scimdirectory.ErrNotFound
	}
	if d.protected != nil {
		return scimdirectory.User{}, d.protected
	}
	d.activeCalls = append(d.activeCalls, active)
	user.Active = active
	d.users[id] = user
	return user, nil
}

func (d *recordingDirectory) ApplyUserChanges(
	_ context.Context, id int, changes scimdirectory.UserChanges,
) (scimdirectory.User, error) {
	user, ok := d.users[id]
	if !ok {
		return scimdirectory.User{}, scimdirectory.ErrNotFound
	}
	if d.protected != nil {
		return scimdirectory.User{}, d.protected
	}
	if changes.UserName != nil && d.conflictOnUserName {
		return scimdirectory.User{}, scimdirectory.ErrConflict
	}
	d.changes = append(d.changes, changes)
	if changes.UserName != nil {
		user.UserName = scimdirectory.NormalizeUserName(*changes.UserName)
	}
	if changes.DisplayName != nil {
		user.DisplayName = *changes.DisplayName
	}
	if changes.ExternalID != nil {
		user.ExternalID = *changes.ExternalID
	}
	if changes.Active != nil {
		d.activeCalls = append(d.activeCalls, *changes.Active)
		user.Active = *changes.Active
	}
	if changes.GivenName != nil {
		user.GivenName = *changes.GivenName
	}
	if changes.FamilyName != nil {
		user.FamilyName = *changes.FamilyName
	}
	if changes.FormattedName != nil {
		user.FormattedName = *changes.FormattedName
	}
	d.users[id] = user
	return user, nil
}

func serve(t *testing.T, directory Directory, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	recorder := httptest.NewRecorder()
	NewHandler(directory).Routes().ServeHTTP(recorder, request)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	return body
}

/* ── the filter is refused, never ignored ──────────────────────────────── */

func TestAnUnsupportedFilterIsRefusedWithItsSCIMCode(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodGet,
		`/Users?filter=`+`userName+eq+%22a%22+or+active+eq+true`, "")

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	body := decodeBody(t, recorder)
	// `invalidFilter` is the code a client switches on. Prose alone makes this
	// indistinguishable from a malformed request.
	require.Equal(t, "invalidFilter", body["scimType"])
	// And the store was never asked: an ignored filter is the defect.
	require.Nil(t, directory.created)
}

func TestASupportedFilterReachesTheStore(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodGet, `/Users?filter=userName+eq+%22alice@corp.com%22`, "")

	require.Equal(t, http.StatusOK, recorder.Code)
	body := decodeBody(t, recorder)
	// Capital R. A client that looks for `resources` finds nothing, which reads
	// to it as an empty directory rather than as a malformed response.
	require.Contains(t, body, "Resources")
	require.EqualValues(t, 1, body["totalResults"])
}

/* ── the resource shape ────────────────────────────────────────────────── */

func TestTheIDIsAString(t *testing.T) {
	recorder := serve(t, newRecordingDirectory(), http.MethodGet, "/Users/42", "")

	require.Equal(t, http.StatusOK, recorder.Code)
	body := decodeBody(t, recorder)
	require.Equal(t, "42", body["id"], "a SCIM id is a string; a number round-trips through a float")
	require.Equal(t, contentType, recorder.Header().Get("Content-Type"))
}

// An empty externalId is OMITTED rather than sent as "". A client that reads
// back an empty value may take it as an instruction to clear its own mapping.
func TestAnAbsentExternalIDIsOmitted(t *testing.T) {
	directory := newRecordingDirectory()
	directory.users[42] = scimdirectory.User{ID: 42, UserName: "alice@corp.com", Active: true}

	body := decodeBody(t, serve(t, directory, http.MethodGet, "/Users/42", ""))
	require.NotContains(t, body, "externalId")
}

func TestAnUnknownUserIsNotFound(t *testing.T) {
	recorder := serve(t, newRecordingDirectory(), http.MethodGet, "/Users/999", "")
	require.Equal(t, http.StatusNotFound, recorder.Code)
}

/* ── create ────────────────────────────────────────────────────────────── */

// A create with no `active` means active. Defaulting to suspended would create
// every account locked out, and an identity provider pushing a new joiner
// rarely states the flag.
func TestACreateWithNoActiveFlagIsActive(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPost, "/Users",
		`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],
		  "userName":"bob@corp.com","displayName":"Bob"}`)

	require.Equal(t, http.StatusCreated, recorder.Code)
	require.Len(t, directory.created, 1)
	require.True(t, directory.created[0].Active)
	require.Equal(t, BasePath+"/Users/43", recorder.Header().Get("Location"))
}

// The handler must carry the DISTINCTION down, not just the value. Without
// ActiveStated the store cannot tell an omitted flag from an explicit one.
func TestTheHandlerReportsWhetherActiveWasStated(t *testing.T) {
	directory := newRecordingDirectory()
	serve(t, directory, http.MethodPost, "/Users", `{"userName":"bob@corp.com"}`)
	require.Len(t, directory.created, 1)
	require.False(t, directory.created[0].ActiveStated,
		"an omitted active must not read as a statement about the person")

	directory = newRecordingDirectory()
	serve(t, directory, http.MethodPost, "/Users", `{"userName":"bob@corp.com","active":true}`)
	require.True(t, directory.created[0].ActiveStated)
	require.True(t, directory.created[0].Active)
}

// Entra ID sends a `userName` that is a UPN and the routable address as the
// primary email. A create with no userName at all must still resolve one.
func TestAPrimaryEmailStandsInForAMissingUserName(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPost, "/Users",
		`{"emails":[{"value":"secondary@corp.com"},{"value":"bob@corp.com","primary":true}]}`)

	require.Equal(t, http.StatusCreated, recorder.Code)
	require.Equal(t, "bob@corp.com", directory.created[0].UserName)
}

// Attributes this platform stores nowhere are DROPPED, not refused. RFC 7643
// requires a service provider to accept a resource carrying attributes it does
// not support, and refusing would break provisioning from every identity
// provider that sends its full default profile.
func TestUnsupportedAttributesDoNotFailTheCreate(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPost, "/Users",
		`{"userName":"bob@corp.com","title":"Engineer","phoneNumbers":[{"value":"+1"}],
		  "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User":{"department":"R&D"}}`)

	require.Equal(t, http.StatusCreated, recorder.Code)
}

func TestACreateWithNoAddressIsRefused(t *testing.T) {
	recorder := serve(t, newRecordingDirectory(), http.MethodPost, "/Users", `{"displayName":"Bob"}`)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, "invalidValue", decodeBody(t, recorder)["scimType"])
}

/* ── PATCH: both shapes in the wild ────────────────────────────────────── */

// The Okta shape.
func TestAPathedActivePatchIsApplied(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPatch, "/Users/42",
		`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		  "Operations":[{"op":"replace","path":"active","value":false}]}`)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []bool{false}, directory.activeCalls)
}

// The Entra ID shape: no path, the attributes in an object. A handler that only
// knew the shape above would silently ignore every deactivation from it.
func TestAPathlessActivePatchIsApplied(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPatch, "/Users/42",
		`{"Operations":[{"op":"Replace","value":{"active":false}}]}`)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []bool{false}, directory.activeCalls)
}

// Some clients send the string "False". Refusing it would leave the account
// active after the provider believed it had deactivated it.
func TestAStringBooleanIsAccepted(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPatch, "/Users/42",
		`{"Operations":[{"op":"replace","path":"active","value":"False"}]}`)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []bool{false}, directory.activeCalls)
}

// A pathless operation carrying only attributes this service does not store is
// understood and changes nothing. Refusing it would stop a provider whose
// profile update happens to travel with the deactivation this handler exists to
// apply.
func TestAPathlessPatchWithNothingToApplyReturnsTheResourceUnchanged(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPatch, "/Users/42",
		`{"Operations":[{"op":"replace","value":{"title":"Engineer"}}]}`)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Empty(t, directory.activeCalls)
	require.Equal(t, true, decodeBody(t, recorder)["active"])
}

/* ── DELETE deactivates ────────────────────────────────────────────────── */

// A DELETE must revoke access. It does NOT remove the row — see the file header
// of users.go — and the test asserts the revocation actually reached the store,
// because a 204 with nothing behind it is the failure that matters here.
func TestADeleteDeactivatesRatherThanAnsweringWithNothingBehindIt(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodDelete, "/Users/42", "")

	require.Equal(t, http.StatusNoContent, recorder.Code)
	require.Equal(t, []bool{false}, directory.activeCalls)
	require.False(t, directory.users[42].Active)
}

/* ── the surfaces that declare what this is ────────────────────────────── */

// The configuration document must report what this handler DOES. One that
// over-reports is how a client comes to send requests the server then fails.
func TestTheServiceProviderConfigReportsWhatIsImplemented(t *testing.T) {
	body := decodeBody(t, serve(t, newRecordingDirectory(), http.MethodGet, "/ServiceProviderConfig", ""))

	require.Equal(t, true, body["patch"].(map[string]any)["supported"])
	require.Equal(t, true, body["filter"].(map[string]any)["supported"])
	require.Equal(t, false, body["bulk"].(map[string]any)["supported"])
	require.Equal(t, false, body["sort"].(map[string]any)["supported"])
	require.Equal(t, false, body["changePassword"].(map[string]any)["supported"])
}

// ResourceTypes and the group catalogue moved to groups_internal_test.go when
// /Groups became a served resource. What is asserted there is the same rule
// read the other way round: the catalogue lists what this tree answers, so a
// client neither misses a resource nor discovers one that refuses everything.

/* ── an unwired handler refuses ────────────────────────────────────────── */

// A SCIM client treats a 2xx as done. An unwired handler answering an empty
// list would tell an identity provider that this deployment has no users and
// that every deactivation had already been applied.
func TestAnUnwiredDirectoryRefusesRatherThanAnsweringEmpty(t *testing.T) {
	recorder := serve(t, nil, http.MethodGet, "/Users", "")
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

/* ── PATCH: the Entra ID interpreter, with real request bodies ─────────── */

const patchOpSchema = `"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"]`

func patchUser(t *testing.T, directory *recordingDirectory, operations string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(t, directory, http.MethodPatch, "/Users/42",
		`{`+patchOpSchema+`,"Operations":[`+operations+`]}`)
}

func str(value string) *string { return &value }

func TestUserPatchBodiesFromEntra(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		operations string
		want       scimdirectory.UserChanges
		wantNone   bool
	}{
		{
			// Documented Entra example: legacy dialect, string boolean.
			name:       "Replace active False string (legacy)",
			operations: `{"op":"Replace","path":"active","value":"False"}`,
			want:       scimdirectory.UserChanges{Active: new(bool)},
		},
		{
			name:       "path-less object with string boolean",
			operations: `{"op":"Replace","value":{"active":"False"}}`,
			want:       scimdirectory.UserChanges{Active: new(bool)},
		},
		{
			name:       "boolean spelled true",
			operations: `{"op":"replace","path":"ACTIVE","value":true}`,
			wantNone:   true, // already active
		},
		{
			// Documented Entra multi-attribute update (aadOptscim062020).
			name: "multiple ops with work email, name parts and enterprise department",
			operations: `
				{"op":"Replace","path":"emails[type eq \"work\"].value","value":"Alice.New@Corp.com"},
				{"op":"Replace","path":"name.givenName","value":"Alicia"},
				{"op":"Replace","path":"name.familyName","value":"Smith"},
				{"op":"Add","path":"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department","value":"R&D"},
				{"op":"Replace","path":"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:manager","value":"00u9"},
				{"op":"Remove","path":"phoneNumbers[type eq \"work\"].value"}`,
			// The work email does NOT re-address the account (userName does),
			// and the name parts do not overwrite the stored display name.
			want: scimdirectory.UserChanges{GivenName: str("Alicia"), FamilyName: str("Smith")},
		},
		{
			name: "path-less object with dotted and URN keys",
			operations: `{"op":"Replace","value":{
				"displayName":"Alicia S","externalId":"00u2",
				"name.givenName":"Alicia",
				"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department":"R&D",
				"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User":{"manager":{"value":"1"}},
				"phoneNumbers":[{"value":"+1","type":"work"}]}}`,
			want: scimdirectory.UserChanges{
				DisplayName: str("Alicia S"), ExternalID: str("00u2"), GivenName: str("Alicia"),
			},
		},
		{
			name:       "path-less nested name object",
			operations: `{"op":"Replace","value":{"name":{"formatted":"Alicia Smith"}}}`,
			want:       scimdirectory.UserChanges{FormattedName: str("Alicia Smith")},
		},
		{
			name:       "a whole emails array replace is accepted and changes nothing",
			operations: `{"op":"replace","path":"emails","value":[{"value":"x@corp.com","type":"home"},{"value":"new@corp.com","type":"work","primary":true}]}`,
			wantNone:   true,
		},
		{
			// CR1: Entra sends a mail change ALONE. Applying it re-addressed the
			// account, Entra's next `userName eq "<upn>"` missed, and it POSTed
			// a duplicate account.
			name:       "an email-only PATCH never re-addresses the account",
			operations: `{"op":"Replace","path":"emails[type eq \"work\"].value","value":"alice.mail@corp.com"}`,
			wantNone:   true,
		},
		{
			name:       "a single familyName change is applied, not dropped",
			operations: `{"op":"Replace","path":"name.familyName","value":"Jones"}`,
			want:       scimdirectory.UserChanges{FamilyName: str("Jones")},
		},
		{
			// CR4: removing `name` removes the parts, never the display name.
			name:       "remove name keeps the display name",
			operations: `{"op":"Remove","path":"name"}`,
			wantNone:   true, // no parts stored, and the display name is untouched
		},
		{
			name:       "a home email is not the address",
			operations: `{"op":"Replace","path":"emails[type eq \"home\"].value","value":"h@corp.com"}`,
			wantNone:   true,
		},
		{
			name:       "userName wins over a work email in the same request",
			operations: `{"op":"Replace","path":"emails[type eq \"work\"].value","value":"mail@corp.com"},{"op":"Replace","path":"userName","value":"upn@corp.com"}`,
			want:       scimdirectory.UserChanges{UserName: str("upn@corp.com")},
		},
		{
			name:       "remove clears an optional attribute",
			operations: `{"op":"Remove","path":"externalId"},{"op":"remove","path":"displayName"}`,
			want:       scimdirectory.UserChanges{ExternalID: str(""), DisplayName: str("")},
		},
		{
			name:       "a null displayName clears it",
			operations: `{"op":"Replace","path":"displayName","value":null}`,
			want:       scimdirectory.UserChanges{DisplayName: str("")},
		},
		{
			name:       "later operations see earlier ones",
			operations: `{"op":"Replace","path":"displayName","value":"A"},{"op":"Replace","path":"displayName","value":"Alice"}`,
			wantNone:   true, // back to the stored value
		},
		{
			name:       "only attributes that are not stored is a no-op 200",
			operations: `{"op":"Add","path":"title","value":"Eng"},{"op":"Replace","path":"addresses[type eq \"work\"].streetAddress","value":"1 Main"}`,
			wantNone:   true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			directory := newRecordingDirectory()
			recorder := patchUser(t, directory, testCase.operations)

			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			if testCase.wantNone {
				require.Empty(t, directory.changes, "nothing stored changed, so nothing is persisted")
				return
			}
			// One request is ONE persist.
			require.Len(t, directory.changes, 1)
			require.Equal(t, testCase.want, directory.changes[0])
		})
	}
}

func TestUserPatchRefusals(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		operations string
		status     int
		scimType   string
	}{
		{"unknown op", `{"op":"move","path":"userName","value":"x"}`, 400, "invalidSyntax"},
		{"remove userName", `{"op":"Remove","path":"userName"}`, 400, "mutability"},
		{"empty userName", `{"op":"Replace","path":"userName","value":""}`, 400, "invalidValue"},
		{"non boolean active", `{"op":"Replace","path":"active","value":"maybe"}`, 400, "invalidValue"},
		{"remove without path", `{"op":"Remove"}`, 400, "noTarget"},
		{"non string displayName", `{"op":"Replace","path":"displayName","value":{"a":1}}`, 400, "invalidValue"},
		// R2: an explicit `active` path with null is not a boolean.
		{"explicit active null", `{"op":"Replace","path":"active","value":null}`, 400, "invalidValue"},
		// L1: the core URN key is read once, at the top level, never recursively.
		{"nested core schema URN", `{"op":"Replace","value":{
			"urn:ietf:params:scim:schemas:core:2.0:User":{
				"urn:ietf:params:scim:schemas:core:2.0:User":{"displayName":"Nested"}}}}`, 400, "invalidPath"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			directory := newRecordingDirectory()
			// A refused operation anywhere must leave the earlier ones unapplied.
			recorder := patchUser(t, directory,
				`{"op":"Replace","path":"displayName","value":"Applied?"},`+testCase.operations)

			require.Equal(t, testCase.status, recorder.Code)
			require.Equal(t, testCase.scimType, decodeBody(t, recorder)["scimType"])
			require.Empty(t, directory.changes)
			require.Equal(t, "Alice", directory.users[42].DisplayName)
		})
	}
}

// R2: encoding/json reads null into a bool as false, so a path-less
// `{"active": null}` used to SUSPEND the account behind a 200. It is ignored.
func TestAPathlessNullActiveIsIgnoredNotReadAsFalse(t *testing.T) {
	for _, operations := range []string{
		`{"op":"replace","value":{"active":null}}`,
		`{"op":"replace","value":{"urn:ietf:params:scim:schemas:core:2.0:User:active":null}}`,
		`{"op":"replace","value":{"urn:ietf:params:scim:schemas:core:2.0:User":{"active":null}}}`,
	} {
		directory := newRecordingDirectory()
		recorder := patchUser(t, directory, operations)

		require.Equal(t, http.StatusOK, recorder.Code, operations)
		require.Empty(t, directory.changes, operations)
		require.Empty(t, directory.activeCalls, operations)
		require.True(t, directory.users[42].Active, operations)
	}
}

func TestTheCoreSchemaURNKeyIsReadAtTheTopLevel(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := patchUser(t, directory, `{"op":"Replace","value":{
		"urn:ietf:params:scim:schemas:core:2.0:User":{"displayName":"Alicia"}}}`)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, []scimdirectory.UserChanges{{DisplayName: str("Alicia")}}, directory.changes)
}

// CR2: Entra maps displayName, givenName and surname independently and sends
// only what changed. The parts are stored and read back as sent, they never
// rewrite a display name the client manages, and a repeat of the same PATCH
// changes nothing — the client converges instead of flapping.
func TestEntraNamePartsConvergeAndNeverRewriteTheDisplayName(t *testing.T) {
	directory := newRecordingDirectory()
	alice := directory.users[42]
	alice.DisplayName = "Smith, John (Contractor)"
	directory.users[42] = alice

	operations := `{"op":"Replace","path":"name.givenName","value":"John"},
		{"op":"Replace","path":"name.familyName","value":"Smith"},
		{"op":"Replace","path":"name.formatted","value":"John Smith"}`
	recorder := patchUser(t, directory, operations)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, "Smith, John (Contractor)", directory.users[42].DisplayName)

	body := decodeBody(t, serve(t, directory, http.MethodGet, "/Users/42", ""))
	require.Equal(t, "Smith, John (Contractor)", body["displayName"])
	require.Equal(t, map[string]any{
		"givenName": "John", "familyName": "Smith", "formatted": "John Smith",
	}, body["name"])

	// The same PATCH again is a no-op: nothing differs any more.
	require.Equal(t, http.StatusOK, patchUser(t, directory, operations).Code)
	require.Len(t, directory.changes, 1, "a repeated name PATCH must not write again")

	// A single part is MERGED with the stored counterpart.
	require.Equal(t, http.StatusOK, patchUser(t, directory,
		`{"op":"Replace","path":"name.familyName","value":"Jones"}`).Code)
	require.Equal(t, "John", directory.users[42].GivenName)
	require.Equal(t, "Jones", directory.users[42].FamilyName)
	require.Equal(t, "Smith, John (Contractor)", directory.users[42].DisplayName)

	// `remove name` clears the parts and leaves the display name alone (CR4).
	require.Equal(t, http.StatusOK, patchUser(t, directory, `{"op":"Remove","path":"name"}`).Code)
	require.Empty(t, directory.users[42].GivenName)
	require.Empty(t, directory.users[42].FamilyName)
	require.Empty(t, directory.users[42].FormattedName)
	require.Equal(t, "Smith, John (Contractor)", directory.users[42].DisplayName)
}

// The parts FILL an empty display name, which is the one case they may.
func TestNamePartsFillAnEmptyDisplayName(t *testing.T) {
	directory := newRecordingDirectory()
	alice := directory.users[42]
	alice.DisplayName = ""
	directory.users[42] = alice

	recorder := patchUser(t, directory, `{"op":"Replace","path":"name.givenName","value":"Alice"},
		{"op":"Replace","path":"name.familyName","value":"Smith"}`)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "Alice Smith", directory.users[42].DisplayName)
}

// The GET derives `formatted` from the stored parts when none was sent, and
// from the display name when there are no parts at all.
func TestTheNameIsRenderedFromWhatWasStored(t *testing.T) {
	directory := newRecordingDirectory()
	body := decodeBody(t, serve(t, directory, http.MethodGet, "/Users/42", ""))
	require.Equal(t, map[string]any{"formatted": "Alice"}, body["name"])

	alice := directory.users[42]
	alice.GivenName, alice.FamilyName = "Alice", "Smith"
	directory.users[42] = alice
	body = decodeBody(t, serve(t, directory, http.MethodGet, "/Users/42", ""))
	require.Equal(t, map[string]any{
		"givenName": "Alice", "familyName": "Smith", "formatted": "Alice Smith",
	}, body["name"])
}

// A create or a replace carries the parts, and says whether its display name
// was composed from them (so the store lets it fill an empty one only).
func TestCreateAndReplaceCarryTheNamePartsAndWhereTheDisplayNameCameFrom(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPost, "/Users",
		`{"userName":"bob@corp.com","name":{"givenName":"Bob","familyName":"Ray"}}`)
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
	created := directory.created[0]
	require.True(t, created.NameStated)
	require.True(t, created.DisplayNameDerived)
	require.Equal(t, "Bob Ray", created.DisplayName)
	require.Equal(t, "Bob", created.GivenName)
	require.Equal(t, "Ray", created.FamilyName)

	recorder = serve(t, directory, http.MethodPut, "/Users/42",
		`{"userName":"alice@corp.com","displayName":"Alice S"}`)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	replaced := directory.replaced[0]
	require.False(t, replaced.DisplayNameDerived)
	require.False(t, replaced.NameStated)
}

// H1: a write the directory refuses as protected is 403 `mutability`, and the
// reason reaches the client so the operator knows where to act instead.
func TestAProtectedAccountIsRefusedWithMutabilityAndTheReason(t *testing.T) {
	directory := newRecordingDirectory()
	directory.protected = &scimdirectory.ProtectedError{
		Reason:        "account holds an administration role — remove the role, then let the IdP retry",
		Administrator: true,
	}

	const deprovision = "SCIM deprovisioning refused: account holds an administration role — " +
		"remove the role, then let the IdP retry"
	const write = "SCIM write refused: account holds an administration role — " +
		"remove the role, then let the IdP retry"
	for _, request := range []struct{ method, body, detail string }{
		{http.MethodPatch, `{"Operations":[{"op":"replace","path":"userName","value":"evil@corp.com"}]}`, write},
		{http.MethodPatch, `{"Operations":[{"op":"replace","path":"active","value":false}]}`, deprovision},
		{http.MethodPut, `{"userName":"evil@corp.com"}`, write},
		{http.MethodPut, `{"userName":"alice@corp.com","active":false}`, deprovision},
		{http.MethodDelete, "", deprovision},
	} {
		// N2: the refusal is ALSO recorded in the audit trail, with the same
		// sentence, on the account it names.
		request2 := httptest.NewRequest(request.method, "/Users/42", strings.NewReader(request.body))
		ctx, slot := audit.ContextWithAnnotationSlot(request2.Context())
		recorder := httptest.NewRecorder()
		NewHandler(directory).Routes().ServeHTTP(recorder, request2.WithContext(ctx))

		require.Equal(t, http.StatusForbidden, recorder.Code, request.method)
		body := decodeBody(t, recorder)
		require.Equal(t, "mutability", body["scimType"], request.method)
		require.Equal(t, request.detail, body["detail"], request.method+" "+request.body)

		annotation, present := slot.Read()
		require.True(t, present, request.method)
		require.Equal(t, request.detail, annotation.Action)
		require.Equal(t, "user", annotation.EntityType)
		require.Equal(t, int64(42), *annotation.EntityID)
	}
	require.Equal(t, "alice@corp.com", directory.users[42].UserName)
}

// Display names and name parts are capped at 256 characters on PATCH, POST
// and PUT alike.
func TestNamesLongerThanTheCapAreRefused(t *testing.T) {
	long := strings.Repeat("é", 257)
	for _, operations := range []string{
		`{"op":"Replace","path":"displayName","value":"` + long + `"}`,
		`{"op":"Replace","path":"name.givenName","value":"` + long + `"}`,
		`{"op":"Replace","value":{"name":{"familyName":"` + long + `"}}}`,
		`{"op":"Replace","path":"name.formatted","value":"` + long + `"}`,
	} {
		directory := newRecordingDirectory()
		recorder := patchUser(t, directory, operations)
		require.Equal(t, http.StatusBadRequest, recorder.Code, operations)
		require.Equal(t, "invalidValue", decodeBody(t, recorder)["scimType"])
		require.Empty(t, directory.changes)
	}
	// Exactly at the cap is accepted.
	require.Equal(t, http.StatusOK, patchUser(t, newRecordingDirectory(),
		`{"op":"Replace","path":"displayName","value":"`+strings.Repeat("é", 256)+`"}`).Code)

	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPost, "/Users",
		`{"userName":"bob@corp.com","name":{"givenName":"`+long+`"}}`)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Empty(t, directory.created)
	recorder = serve(t, directory, http.MethodPut, "/Users/42",
		`{"userName":"alice@corp.com","displayName":"`+long+`"}`)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Empty(t, directory.replaced)
}

func TestUserPatchUniquenessConflictIsAScimConflict(t *testing.T) {
	directory := newRecordingDirectory()
	directory.conflictOnUserName = true
	recorder := patchUser(t, directory, `{"op":"Replace","path":"userName","value":"Bob@corp.com"}`)

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Equal(t, "uniqueness", decodeBody(t, recorder)["scimType"])
}

func TestUserPatchOnAnUnknownUserIsNotFound(t *testing.T) {
	recorder := serve(t, newRecordingDirectory(), http.MethodPatch, "/Users/999",
		`{"Operations":[{"op":"replace","path":"active","value":false}]}`)
	require.Equal(t, http.StatusNotFound, recorder.Code)
}

/* ── filters ───────────────────────────────────────────────────────────── */

// Entra's connection test: no such user is an EMPTY ListResponse, not an error.
func TestEntraTestConnectionFilterAnswersAnEmptyListResponse(t *testing.T) {
	directory := newRecordingDirectory()
	directory.users = map[int]scimdirectory.User{}
	recorder := serve(t, directory, http.MethodGet,
		`/Users?filter=userName+eq+%22b4f4e5e9-2cf5-4f1c-a8c0-0e3c0e5b8c11%22&excludedAttributes=members`, "")

	require.Equal(t, http.StatusOK, recorder.Code)
	body := decodeBody(t, recorder)
	require.EqualValues(t, 0, body["totalResults"])
	require.Equal(t, []any{}, body["Resources"])
}

func TestFilterValuesMayContainOperatorsAndBrackets(t *testing.T) {
	for _, filter := range []string{
		`userName eq "o(brien) and sons@corp.com"`,
		`displayName eq "a [b] or not(c)"`,
		`userName eq "x" and active eq true`,
		`externalId eq "00u1" and userName eq "a and b"`,
	} {
		directory := newRecordingDirectory()
		recorder := serve(t, directory, http.MethodGet, "/Users?filter="+url.QueryEscape(filter), "")
		require.Equal(t, http.StatusOK, recorder.Code, filter)
	}
}

/* ── attributes / excludedAttributes ───────────────────────────────────── */

func TestExcludedAttributesAndAttributesAreHonouredOnUsers(t *testing.T) {
	directory := newRecordingDirectory()

	body := decodeBody(t, serve(t, directory, http.MethodGet, "/Users/42?excludedAttributes=emails,name", ""))
	require.NotContains(t, body, "emails")
	require.NotContains(t, body, "name")
	require.Contains(t, body, "userName")

	body = decodeBody(t, serve(t, directory, http.MethodGet, "/Users/42?attributes=userName", ""))
	require.Equal(t, "alice@corp.com", body["userName"])
	require.Contains(t, body, "id")
	require.NotContains(t, body, "emails")
	require.NotContains(t, body, "displayName")
}

/* ── discovery ─────────────────────────────────────────────────────────── */

func TestDiscoveryListsNameSubAttributesAndTheEnterpriseExtension(t *testing.T) {
	directory := newRecordingDirectory()

	schemas := decodeBody(t, serve(t, directory, http.MethodGet, "/Schemas", ""))
	ids := map[string]map[string]any{}
	for _, resource := range schemas["Resources"].([]any) {
		document := resource.(map[string]any)
		ids[document["id"].(string)] = document
	}
	require.Contains(t, ids, schemaEnterprise)

	var name map[string]any
	for _, attribute := range ids[schemaUser]["attributes"].([]any) {
		if attribute.(map[string]any)["name"] == "name" {
			name = attribute.(map[string]any)
		}
	}
	require.NotNil(t, name)
	subNames := []string{}
	for _, sub := range name["subAttributes"].([]any) {
		subNames = append(subNames, sub.(map[string]any)["name"].(string))
	}
	require.ElementsMatch(t, []string{"formatted", "givenName", "familyName"}, subNames)

	types := decodeBody(t, serve(t, directory, http.MethodGet, "/ResourceTypes", ""))
	for _, resource := range types["Resources"].([]any) {
		document := resource.(map[string]any)
		if document["id"] == "User" {
			extensions := document["schemaExtensions"].([]any)
			require.Equal(t, schemaEnterprise, extensions[0].(map[string]any)["schema"])
			require.Equal(t, false, extensions[0].(map[string]any)["required"])
		}
	}
}

func TestMetaLocationsOfDiscoveryDocumentsResolve(t *testing.T) {
	directory := newRecordingDirectory()
	for _, listing := range []string{"/Schemas", "/ResourceTypes"} {
		body := decodeBody(t, serve(t, directory, http.MethodGet, listing, ""))
		for _, resource := range body["Resources"].([]any) {
			location := resource.(map[string]any)["meta"].(map[string]any)["location"].(string)
			recorder := serve(t, directory, http.MethodGet, strings.TrimPrefix(location, BasePath), "")
			require.Equal(t, http.StatusOK, recorder.Code, location)
			require.Equal(t, resource.(map[string]any)["id"], decodeBody(t, recorder)["id"])
		}
	}
	require.Equal(t, http.StatusNotFound, serve(t, directory, http.MethodGet, "/Schemas/nope", "").Code)
	require.Equal(t, http.StatusNotFound, serve(t, directory, http.MethodGet, "/ResourceTypes/nope", "").Code)
}
