package scim

// The RFC 7644 §3.5.2 PatchOp interpreter for `/Users`.
//
// # What it is for
//
// Microsoft Entra ID provisions with PATCH for everything after the create: a
// rename, a mail change, a manager change, a deactivation. Two dialects are in
// the wild and both are read here.
//
//   - The compliant one (`aadOptscim062020`): `Add`/`Replace`/`Remove` with a
//     `path`, and `active` as a JSON boolean.
//   - The legacy one: a path-less `Replace` whose value is an object, dotted or
//     URN-prefixed keys, and `active` as the STRING "False".
//
// A PATCH that failed on the first attribute it did not store used to fail the
// whole request, and Entra then quarantined the app. So the rule is split in
// two: an attribute this service STORES is applied exactly, and one it does not
// store (the enterprise department, a manager, phone numbers) is accepted and
// dropped, which is the RFC 7643 §3.3 stance create already takes. What is never
// done is accepting a value this service DOES act on and then not applying it.
// The one deliberate exception is a work email: the account's single address is
// its userName, and an email PATCH is accepted as a no-op rather than allowed to
// re-address the account (see userPatch).
//
// # All or nothing
//
// Every operation is applied to an in-memory copy of the account and the result
// is persisted ONCE, in one transaction. A request whose third operation is
// refused changes nothing, because the client will resend the whole request.

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/scimdirectory"
)

const (
	userSchemaPrefix       = schemaUser + ":"
	enterpriseUserSchema   = "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"
	enterpriseSchemaPrefix = enterpriseUserSchema + ":"
)

// patchProblem is a refusal: a SCIM error code and the reason.
type patchProblem struct {
	status   int
	scimType string
	detail   string
}

// userPatch is the working copy the operations are applied to.
//
// # The address changes ONLY through userName
//
// An email operation never re-addresses the account. Entra ID sends the UPN as
// `userName` and the `mail` attribute as the work email, and it sends only the
// attributes that changed — so a mail change arrives ALONE. Applying it to the
// address flipped the account's userName to the mail value, Entra's next
// `userName eq "<upn>"` lookup missed, and it POSTed a duplicate account. Create
// and PUT already let userName win; PATCH now agrees. The email operation is
// still READ (a malformed value is refused) and then accepted as a no-op: the
// platform stores one address per account, and that address is the userName.
// userResource documents what the client reads back.
//
// # The display name and the name parts
//
// `displayName` is the name the platform shows, and the client manages it
// directly. The `name` parts (given, family, formatted) are stored as sent, on
// the SCIM side table, and returned as sent by GET — so a client comparing its
// source with the resource sees no difference and stops re-sending them. The
// parts NEVER overwrite a stored display name ("Smith, John (Contractor)" is
// not rewritten to "John Smith" on every sync); they only fill an EMPTY one.
// A single part is merged with the stored counterpart, so `name.familyName`
// alone is applied, not dropped.
type userPatch struct {
	userName    string
	displayName string
	externalID  string
	active      bool

	userNameStated    bool
	displayNameStated bool

	// The name parts, starting from what is stored.
	given, family, formatted string
}

func newUserPatch(user scimdirectory.User) *userPatch {
	return &userPatch{
		userName: user.UserName, displayName: user.DisplayName,
		externalID: user.ExternalID, active: user.Active,
		given: user.GivenName, family: user.FamilyName, formatted: user.FormattedName,
	}
}

// changes diffs the working copy against the account it started from.
func (p *userPatch) changes(before scimdirectory.User) (scimdirectory.UserChanges, bool) {
	var changes scimdirectory.UserChanges
	changed := false

	if p.userNameStated &&
		scimdirectory.NormalizeUserName(p.userName) != scimdirectory.NormalizeUserName(before.UserName) {
		address := p.userName
		changes.UserName, changed = &address, true
	}

	if p.given != before.GivenName {
		given := p.given
		changes.GivenName, changed = &given, true
	}
	if p.family != before.FamilyName {
		family := p.family
		changes.FamilyName, changed = &family, true
	}
	if p.formatted != before.FormattedName {
		formatted := p.formatted
		changes.FormattedName, changed = &formatted, true
	}

	name := before.DisplayName
	switch {
	case p.displayNameStated:
		name = p.displayName
	case before.DisplayName == "":
		// Only an EMPTY display name is filled from the parts.
		name = strings.TrimSpace(p.formatted)
		if name == "" {
			name = strings.TrimSpace(p.given + " " + p.family)
		}
	}
	if name != before.DisplayName {
		changes.DisplayName, changed = &name, true
	}
	if p.externalID != before.ExternalID {
		external := p.externalID
		changes.ExternalID, changed = &external, true
	}
	if p.active != before.Active {
		active := p.active
		changes.Active, changed = &active, true
	}
	return changes, changed
}

// applyUserOperations interprets every operation, or returns the first refusal.
func applyUserOperations(patch *userPatch, operations []patchOperation) *patchProblem {
	for _, operation := range operations {
		op := strings.ToLower(strings.TrimSpace(operation.Op))
		if op != "add" && op != "replace" && op != "remove" {
			return &patchProblem{400, "invalidSyntax",
				"the operation " + quoteForError(operation.Op) + " is not add, replace or remove"}
		}
		path := strings.TrimSpace(operation.Path)
		if path == "" {
			if op == "remove" {
				return &patchProblem{400, "noTarget", "a remove operation needs a path"}
			}
			if problem := patch.applyObject(op, operation.Value, false); problem != nil {
				return problem
			}
			continue
		}
		if problem := patch.applyPath(op, path, operation.Value, false); problem != nil {
			return problem
		}
	}
	return nil
}

// applyObject reads a path-less add/replace: an object of attributes, whose keys
// are paths in their own right (`name.givenName`, a URN-prefixed attribute, or a
// plain attribute whose value may itself be an object such as `name`).
//
// `nested` is true inside a core-schema URN object or a `name` object. The URN
// key is honoured ONLY at the top level, so a value cannot nest
// `{"urn:…:User":{"urn:…:User":{…}}}` and have every level re-read.
func (p *userPatch) applyObject(op string, raw json.RawMessage, nested bool) *patchProblem {
	var attributes map[string]json.RawMessage
	if err := json.Unmarshal(raw, &attributes); err != nil {
		return &patchProblem{400, "invalidValue", "a path-less operation needs an object value"}
	}
	for key, value := range attributes {
		if isJSONNull(value) && isActivePath(key) {
			// A path-less `{"active": null}` says nothing about the flag and
			// is IGNORED, as it was before this interpreter existed. It must
			// never read as false: that suspended the account behind a 200.
			// An explicit `path: "active"` with null is refused instead.
			continue
		}
		if problem := p.applyPath(op, key, value, nested); problem != nil {
			return problem
		}
	}
	return nil
}

// applyPath applies one operation to one attribute path.
func (p *userPatch) applyPath(op, rawPath string, value json.RawMessage, nested bool) *patchProblem {
	path := strings.TrimSpace(rawPath)
	lowered := strings.ToLower(path)

	// The enterprise extension is not stored. Accepted, and never a reason to
	// fail the request. Checked BEFORE the core prefix is stripped, and for the
	// bare schema URN too (the key of a path-less object).
	if lowered == strings.ToLower(enterpriseUserSchema) ||
		strings.HasPrefix(lowered, strings.ToLower(enterpriseSchemaPrefix)) {
		return nil
	}
	if strings.HasPrefix(lowered, strings.ToLower(userSchemaPrefix)) {
		path = path[len(userSchemaPrefix):]
		lowered = strings.ToLower(path)
	} else if lowered == strings.ToLower(schemaUser) {
		// `{"urn:...:core:2.0:User": {...}}`: the object holds core attributes.
		if nested {
			return &patchProblem{400, "invalidPath",
				"the core User schema URN is read only as a top-level key"}
		}
		return p.applyObject(op, value, true)
	}

	switch {
	case lowered == "username":
		return p.setString(op, value, "userName", true, func(s string) {
			p.userName, p.userNameStated = s, true
		})

	case lowered == "displayname":
		return p.setString(op, value, "displayName", false, func(s string) {
			p.displayName, p.displayNameStated = s, true
		})

	case lowered == "externalid":
		return p.setString(op, value, "externalId", false, func(s string) { p.externalID = s })

	case lowered == "active":
		if op == "remove" {
			return nil
		}
		active, ok := coerceBool(value)
		if !ok {
			return &patchProblem{400, "invalidValue", "the active attribute needs a boolean value"}
		}
		p.active = active
		return nil

	case lowered == "name":
		if op == "remove" {
			// Removes the name PARTS. The display name is a separate
			// attribute the client manages through `displayName`, and
			// removing `name` must not blank it.
			p.given, p.family, p.formatted = "", "", ""
			return nil
		}
		return p.applyObject(op, rewriteKeys(value, "name."), true)

	case lowered == "name.formatted":
		return p.setString(op, value, "name.formatted", false, func(s string) { p.formatted = s })
	case lowered == "name.givenname":
		return p.setString(op, value, "name.givenName", false, func(s string) { p.given = s })
	case lowered == "name.familyname":
		return p.setString(op, value, "name.familyName", false, func(s string) { p.family = s })

	case lowered == "emails" || strings.HasPrefix(lowered, "emails["):
		return p.applyEmails(op, lowered, value)
	}

	// Anything else — `title`, `phoneNumbers[...]`, `addresses`, `name.middleName`,
	// `preferredLanguage`, `roles`, … — is not stored here.
	return nil
}

// setString applies add/replace/remove to a scalar string attribute.
func (p *userPatch) setString(
	op string, value json.RawMessage, name string, required bool, set func(string),
) *patchProblem {
	if op == "remove" {
		if required {
			return &patchProblem{400, "mutability", "the " + name + " attribute cannot be removed"}
		}
		set("")
		return nil
	}
	if len(value) == 0 || string(value) == "null" {
		if required {
			return &patchProblem{400, "invalidValue", "the " + name + " attribute needs a value"}
		}
		set("")
		return nil
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return &patchProblem{400, "invalidValue", "the " + name + " attribute needs a string value"}
	}
	text = strings.TrimSpace(text)
	if required && text == "" {
		return &patchProblem{400, "invalidValue", "the " + name + " attribute needs a value"}
	}
	if problem := nameTooLong(name, text); problem != nil {
		return problem
	}
	set(text)
	return nil
}

// applyEmails reads `emails`, `emails[type eq "work"]` and
// `emails[type eq "work"].value`. The platform stores ONE address per account,
// so an entry that is not the work/primary one is accepted and not stored.
func (p *userPatch) applyEmails(op, loweredPath string, value json.RawMessage) *patchProblem {
	if op == "remove" {
		// The address is the account's identity; clearing it is not meaningful,
		// and failing the request would wedge a provider that maps a nullable
		// mail attribute.
		return nil
	}
	if strings.HasPrefix(loweredPath, "emails[") {
		if other := emailFilterIsOtherType(loweredPath); other {
			return nil
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			// `emails[type eq "work"]` replaced with an object.
			var entry struct {
				Value string `json:"value"`
			}
			if json.Unmarshal(value, &entry) != nil {
				return &patchProblem{400, "invalidValue", "the email value needs a string"}
			}
			text = entry.Value
		}
		return p.setEmail(text)
	}

	// Whole-array form, or a single object.
	var entries []struct {
		Value   string `json:"value"`
		Primary bool   `json:"primary"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal(value, &entries); err != nil {
		var single struct {
			Value   string `json:"value"`
			Primary bool   `json:"primary"`
			Type    string `json:"type"`
		}
		if json.Unmarshal(value, &single) != nil {
			return &patchProblem{400, "invalidValue", "the emails value needs an array of email objects"}
		}
		entries = append(entries, single)
	}
	chosen := ""
	for _, entry := range entries {
		if strings.TrimSpace(entry.Value) == "" {
			continue
		}
		switch {
		case entry.Primary || strings.EqualFold(entry.Type, "work"):
			chosen = entry.Value
		case chosen == "":
			chosen = entry.Value
		}
		if entry.Primary {
			break
		}
	}
	if chosen == "" {
		return nil
	}
	return p.setEmail(chosen)
}

// setEmail accepts a work/primary email value and stores nothing: the address
// changes only through userName. See the userPatch comment for why applying it
// duplicated accounts under Entra ID.
func (p *userPatch) setEmail(string) *patchProblem {
	return nil
}

// emailFilterIsOtherType reports whether a bracketed path selects an entry whose
// type is stated and is not "work" (home, other).
func emailFilterIsOtherType(loweredPath string) bool {
	open := strings.Index(loweredPath, "[")
	end := strings.LastIndex(loweredPath, "]")
	if open < 0 || end < open {
		return false
	}
	tokens, err := tokenizePathFilter(loweredPath[open+1 : end])
	if err != nil || len(tokens) != 3 || tokens[0] != "type" || tokens[1] != "eq" {
		return false
	}
	return tokens[2] != "work"
}

// tokenizePathFilter splits `type eq "work"` into three words, quotes removed.
func tokenizePathFilter(expression string) ([]string, error) {
	fields := strings.Fields(expression)
	for i := range fields {
		fields[i] = strings.Trim(fields[i], `"`)
	}
	return fields, nil
}

// rewriteKeys prefixes every key of an object value, so `name`'s
// `{"givenName":"A"}` is read as the path `name.givenName`. A non-object value
// is returned unchanged and fails in applyObject with the right message.
func rewriteKeys(raw json.RawMessage, prefix string) json.RawMessage {
	var attributes map[string]json.RawMessage
	if err := json.Unmarshal(raw, &attributes); err != nil {
		return raw
	}
	rewritten := make(map[string]json.RawMessage, len(attributes))
	for key, value := range attributes {
		rewritten[prefix+key] = value
	}
	out, err := json.Marshal(rewritten)
	if err != nil {
		return raw
	}
	return out
}

// coerceBool reads a boolean the way identity providers send it: a JSON
// boolean, or the string "True"/"False" in any case. Refusing the string would
// leave an account active after the provider believed it had deactivated it.
//
// JSON null (or an absent value) is NOT a boolean. encoding/json leaves the
// target untouched for null, so without this check null read as false and
// suspended the account.
func coerceBool(raw json.RawMessage) (value, ok bool) {
	if isJSONNull(raw) {
		return false, false
	}
	var boolean bool
	if json.Unmarshal(raw, &boolean) == nil {
		return boolean, true
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// maxNameLength caps displayName and every `name` sub-attribute, in
// characters. A person's name fits with room to spare; the cap keeps a machine
// credential from storing (and every listing from returning) an arbitrary
// string, and is the same on PATCH, POST and PUT.
const maxNameLength = 256

// nameTooLong refuses a display name or name part over maxNameLength. Other
// attributes are not capped here.
func nameTooLong(attribute, value string) *patchProblem {
	lowered := strings.ToLower(attribute)
	if lowered != "displayname" && !strings.HasPrefix(lowered, "name.") {
		return nil
	}
	if utf8.RuneCountInString(value) <= maxNameLength {
		return nil
	}
	return &patchProblem{400, "invalidValue",
		"the " + attribute + " attribute is longer than " + strconv.Itoa(maxNameLength) + " characters"}
}

// isJSONNull reports an absent value or a JSON null.
func isJSONNull(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "null"
}

// isActivePath reports whether a path-less object key names `active`, bare or
// core-schema-prefixed.
func isActivePath(key string) bool {
	lowered := strings.ToLower(strings.TrimSpace(key))
	return strings.TrimPrefix(lowered, strings.ToLower(userSchemaPrefix)) == "active"
}

func quoteForError(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
