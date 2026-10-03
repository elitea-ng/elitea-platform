package scim

// What these tests hold in place on the /Groups surface.
//
// The dangerous failures here are the ones where the response is a success and
// the access is wrong:
//
//   - A push of an UNBOUND group that was accepted. Nothing on this deployment
//     would say which project it granted, so the 201 would be a lie in both
//     directions: the identity provider records the group as provisioned, and
//     nobody got anything.
//   - A member value that could not be resolved and was DROPPED. The provider
//     is told the whole group is provisioned, and the people it dropped have no
//     access that anybody will look for again.
//   - A PATCH shape that was answered 200 and applied nothing — in particular
//     `members[value eq "…"]`, which is how Entra ID removes one person. Every
//     leaver would stay in the project.
//   - A partly applied PATCH: the request is retried whole, so an operation
//     applied before a refusal is applied twice or, worse, kept when the client
//     believes the whole request failed.
//
// The directory is a FAKE that records what it was asked to do, so each is
// asserted against the store call, not only against the status.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/scimdirectory"
)

/* ── the fake ──────────────────────────────────────────────────────────── */

type fakeBinding struct {
	group   scimdirectory.Group
	members []int
}

type groupState struct {
	bindings map[int64]*fakeBinding
	// The calls, in the order the handler made them.
	adopted     []string
	externalIDs []string
	renamed     []string
	added       [][]int
	replaced    [][]int
	removed     [][]int
	deleted     []int64
	applied     [][]scimdirectory.GroupOperation
	// order records the create path's two writes in the order they arrived, so
	// a test can assert which one a failure would have left behind.
	order  []string
	filter scimdirectory.Filter
}

func newGroupState() *groupState {
	return &groupState{bindings: map[int64]*fakeBinding{
		7: {group: scimdirectory.Group{
			ID: 7, DisplayName: "Platform Team", ProjectID: 12,
			ProjectName: "Platform", RoleName: "editor",
			CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(2, 0),
		}},
	}}
}

// members renders the ledger the way the store would return it.
func (b *fakeBinding) resolved() scimdirectory.Group {
	group := b.group
	group.Members = nil
	for _, member := range b.members {
		group.Members = append(group.Members, scimdirectory.GroupMember{
			UserID: member, UserName: "alice@corp.com", Granted: true,
		})
	}
	return group
}

func (d *recordingDirectory) ListGroups(
	_ context.Context, filter scimdirectory.Filter, _, _ int,
) ([]scimdirectory.Group, int, error) {
	d.groups.filter = filter
	groups := make([]scimdirectory.Group, 0, len(d.groups.bindings))
	for _, binding := range d.groups.bindings {
		groups = append(groups, binding.resolved())
	}
	return groups, len(groups), nil
}

func (d *recordingDirectory) GetGroup(_ context.Context, id int64) (scimdirectory.Group, error) {
	binding, ok := d.groups.bindings[id]
	if !ok {
		return scimdirectory.Group{}, scimdirectory.ErrNotFound
	}
	return binding.resolved(), nil
}

func (d *recordingDirectory) LookupGroup(
	_ context.Context, externalID, displayName string,
) (scimdirectory.Group, error) {
	for _, binding := range d.groups.bindings {
		if externalID != "" && binding.group.ExternalID == externalID {
			return binding.resolved(), nil
		}
		if displayName != "" && binding.group.DisplayName == displayName {
			return binding.resolved(), nil
		}
	}
	return scimdirectory.Group{}, scimdirectory.ErrNoBinding
}

func (d *recordingDirectory) AdoptGroup(_ context.Context, id int64, externalID, displayName string) error {
	binding, ok := d.groups.bindings[id]
	if !ok {
		return scimdirectory.ErrNotFound
	}
	d.groups.adopted = append(d.groups.adopted, externalID)
	d.groups.order = append(d.groups.order, "adopt")
	if binding.group.ExternalID == "" {
		binding.group.ExternalID = externalID
	}
	if displayName != "" {
		binding.group.DisplayName = displayName
	}
	return nil
}

func (d *recordingDirectory) RenameGroup(
	_ context.Context, id int64, displayName string,
) (scimdirectory.Group, error) {
	binding, ok := d.groups.bindings[id]
	if !ok {
		return scimdirectory.Group{}, scimdirectory.ErrNotFound
	}
	d.groups.renamed = append(d.groups.renamed, displayName)
	binding.group.DisplayName = displayName
	return binding.resolved(), nil
}

func (d *recordingDirectory) AddGroupMembers(
	_ context.Context, id int64, members []int,
) (scimdirectory.Group, error) {
	binding, ok := d.groups.bindings[id]
	if !ok {
		return scimdirectory.Group{}, scimdirectory.ErrNotFound
	}
	d.groups.added = append(d.groups.added, members)
	binding.members = append(binding.members, members...)
	return binding.resolved(), nil
}

func (d *recordingDirectory) ReplaceGroupMembers(
	_ context.Context, id int64, members []int,
) (scimdirectory.Group, error) {
	binding, ok := d.groups.bindings[id]
	if !ok {
		return scimdirectory.Group{}, scimdirectory.ErrNotFound
	}
	d.groups.replaced = append(d.groups.replaced, members)
	d.groups.order = append(d.groups.order, "members")
	binding.members = members
	return binding.resolved(), nil
}

func (d *recordingDirectory) RemoveGroupMembers(
	_ context.Context, id int64, members []int,
) (scimdirectory.Group, error) {
	binding, ok := d.groups.bindings[id]
	if !ok {
		return scimdirectory.Group{}, scimdirectory.ErrNotFound
	}
	d.groups.removed = append(d.groups.removed, members)
	return binding.resolved(), nil
}

// ApplyGroupOperations records the operations of ONE patch, and applies them
// through the same recorders the single-operation calls use — so a test can
// still assert what reached the store, and a test can also assert that the
// whole patch arrived as one call.
func (d *recordingDirectory) ApplyGroupOperations(
	ctx context.Context, id int64, operations []scimdirectory.GroupOperation,
) (scimdirectory.Group, error) {
	d.groups.applied = append(d.groups.applied, operations)
	group, err := d.GetGroup(ctx, id)
	if err != nil {
		return scimdirectory.Group{}, err
	}
	for _, operation := range operations {
		switch operation.Kind {
		case scimdirectory.GroupRename:
			group, err = d.RenameGroup(ctx, id, operation.DisplayName)
		case scimdirectory.GroupSetExternalID:
			d.groups.bindings[id].group.ExternalID = operation.ExternalID
			d.groups.externalIDs = append(d.groups.externalIDs, operation.ExternalID)
			group, err = d.GetGroup(ctx, id)
		case scimdirectory.GroupAddMembers:
			group, err = d.AddGroupMembers(ctx, id, operation.Members)
		case scimdirectory.GroupReplaceMembers:
			group, err = d.ReplaceGroupMembers(ctx, id, operation.Members)
		case scimdirectory.GroupRemoveMembers:
			group, err = d.RemoveGroupMembers(ctx, id, operation.Members)
		}
		if err != nil {
			return scimdirectory.Group{}, err
		}
	}
	return group, nil
}

func (d *recordingDirectory) DeleteGroup(_ context.Context, id int64) error {
	if _, ok := d.groups.bindings[id]; !ok {
		return scimdirectory.ErrNotFound
	}
	d.groups.deleted = append(d.groups.deleted, id)
	delete(d.groups.bindings, id)
	return nil
}

// ResolveMember answers for the three identifiers the store matches on, plus
// the two refusals.
func (d *recordingDirectory) ResolveMember(_ context.Context, value string) (int, error) {
	switch value {
	case "42", "alice@corp.com", "00u1abc":
		return 42, nil
	case "43":
		return 43, nil
	case "ambiguous":
		return 0, scimdirectory.AmbiguousMemberError{Value: value}
	default:
		return 0, scimdirectory.UnknownMemberError{Value: value}
	}
}

/* ── an unbound group is refused, and grants nothing ───────────────────── */

func TestAPushOfAnUnboundGroupIsRefusedAndProvisionsNothing(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPost, "/Groups",
		`{"displayName":"Finance","members":[{"value":"42"}]}`)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	body := decodeBody(t, recorder)
	// The group is NAMED. An operator reading their provider's log has to be
	// able to tell which of their groups has no binding.
	require.Contains(t, body["detail"], `"Finance"`)

	// And nothing happened. This is the assertion that matters: a refusal that
	// had already granted the members would be the failure this surface exists
	// to prevent.
	require.Empty(t, directory.groups.replaced)
	require.Empty(t, directory.groups.added)
	require.Empty(t, directory.groups.adopted)
}

func TestAPushOfABoundGroupBindsAndReplacesItsMembership(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPost, "/Groups",
		`{"displayName":"Platform Team","externalId":"grp-1","members":[{"value":"42"},{"value":"43"}]}`)

	require.Equal(t, http.StatusCreated, recorder.Code)
	require.Equal(t, "/api/v2/scim/v2/Groups/7", recorder.Header().Get("Location"))
	require.Equal(t, []string{"grp-1"}, directory.groups.adopted)
	// A create states the whole membership, so it replaces rather than adds.
	require.Equal(t, [][]int{{42, 43}}, directory.groups.replaced)

	body := decodeBody(t, recorder)
	require.Equal(t, "7", body["id"])
	grant, ok := body[schemaProjectGrant].(map[string]any)
	require.True(t, ok, "the project grant extension is returned on every group")
	require.EqualValues(t, 12, grant["projectId"])
	require.Equal(t, "editor", grant["role"])
}

/* ── a member this service cannot resolve stops the whole push ─────────── */

func TestAnUnresolvableMemberRefusesTheWholePushRatherThanDroppingThem(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPost, "/Groups",
		`{"displayName":"Platform Team","members":[{"value":"42"},{"value":"nobody@corp.com"}]}`)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, decodeBody(t, recorder)["detail"], `"nobody@corp.com"`)
	// The resolvable member was NOT applied. A partial apply behind a 4xx is
	// the state neither side can see.
	require.Empty(t, directory.groups.replaced)
}

func TestAnAmbiguousMemberValueIsRefusedRatherThanChosenBetween(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPost, "/Groups",
		`{"displayName":"Platform Team","members":[{"value":"ambiguous"}]}`)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, decodeBody(t, recorder)["detail"], "more than one account")
	require.Empty(t, directory.groups.replaced)
}

func TestAMemberWithNoValueIsRefused(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPost, "/Groups",
		`{"displayName":"Platform Team","members":[{"display":"Alice"}]}`)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Empty(t, directory.groups.replaced)
}

/* ── PATCH: the shapes providers really send ───────────────────────────── */

func TestTheBracketedMemberRemovalEntraSendsIsApplied(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPatch, "/Groups/7",
		`{"Operations":[{"op":"remove","path":"members[value eq \"42\"]"}]}`)

	require.Equal(t, http.StatusOK, recorder.Code)
	// The assertion is the STORE CALL. A 200 with no removal is exactly the
	// defect: the provider records the leaver as removed and never resends it.
	require.Equal(t, [][]int{{42}}, directory.groups.removed)
}

func TestAMemberAddAndAMemberReplaceReachTheirOwnOperations(t *testing.T) {
	directory := newRecordingDirectory()

	recorder := serve(t, directory, http.MethodPatch, "/Groups/7",
		`{"Operations":[{"op":"add","path":"members","value":[{"value":"42"}]}]}`)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, [][]int{{42}}, directory.groups.added)
	require.Empty(t, directory.groups.replaced, "an add must not replace the membership")

	recorder = serve(t, directory, http.MethodPatch, "/Groups/7",
		`{"Operations":[{"op":"replace","path":"members","value":[{"value":"43"}]}]}`)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, [][]int{{43}}, directory.groups.replaced)
}

func TestARemoveOfEveryMemberEmptiesTheGroup(t *testing.T) {
	directory := newRecordingDirectory()
	directory.groups.bindings[7].members = []int{42, 43}

	recorder := serve(t, directory, http.MethodPatch, "/Groups/7",
		`{"Operations":[{"op":"remove","path":"members"}]}`)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, [][]int{nil}, directory.groups.replaced)
	require.Empty(t, directory.groups.bindings[7].members)
}

func TestADisplayNameChangeIsAppliedFromBothPatchShapes(t *testing.T) {
	directory := newRecordingDirectory()

	recorder := serve(t, directory, http.MethodPatch, "/Groups/7",
		`{"Operations":[{"op":"replace","path":"displayName","value":"Platform"}]}`)
	require.Equal(t, http.StatusOK, recorder.Code)

	// The pathless shape, which is what several providers send.
	recorder = serve(t, directory, http.MethodPatch, "/Groups/7",
		`{"Operations":[{"op":"replace","value":{"displayName":"Core Platform"}}]}`)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []string{"Platform", "Core Platform"}, directory.groups.renamed)
}

func TestAnUnsupportedPatchPathIsRefusedByNameAndAppliesNothing(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPatch, "/Groups/7",
		`{"Operations":[{"op":"replace","path":"description","value":"grp-2"}]}`)

	require.Equal(t, http.StatusNotImplemented, recorder.Code)
	body := decodeBody(t, recorder)
	require.Equal(t, "invalidPath", body["scimType"])
	require.Contains(t, body["detail"], `"description"`)
	require.Empty(t, directory.groups.renamed)
	require.Empty(t, directory.groups.added)
}

func TestAPatchIsUnderstoodWholeBeforeAnyOfItIsApplied(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPatch, "/Groups/7", `{"Operations":[
		{"op":"replace","path":"displayName","value":"Renamed"},
		{"op":"replace","path":"members[value eq \"42\"]","value":[]}
	]}`)

	require.Equal(t, http.StatusNotImplemented, recorder.Code)
	// The rename came FIRST and was still not applied. A client retries the
	// whole PATCH, and a half-applied one is a state it never asked for.
	require.Empty(t, directory.groups.renamed)
	require.Equal(t, "Platform Team", directory.groups.bindings[7].group.DisplayName)
}

func TestAPatchThatSaysNothingThisServiceStoresChangesNothing(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPatch, "/Groups/7",
		`{"Operations":[{"op":"replace","value":{"description":"the platform team"}}]}`)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Empty(t, directory.groups.renamed)
	require.Empty(t, directory.groups.replaced)
}

/* ── PUT and DELETE ────────────────────────────────────────────────────── */

func TestAPutReplacesTheMembershipAndKeepsTheBinding(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPut, "/Groups/7",
		`{"displayName":"Platform Team","members":[{"value":"alice@corp.com"}]}`)

	require.Equal(t, http.StatusOK, recorder.Code)
	// The address resolved to the account id, and the id is what the resource
	// returns — a client matches on the value, never on the display.
	require.Equal(t, [][]int{{42}}, directory.groups.replaced)
	body := decodeBody(t, recorder)
	members, ok := body["members"].([]any)
	require.True(t, ok)
	require.Equal(t, "42", members[0].(map[string]any)["value"])
}

func TestADeleteWithdrawsTheGroupAndSaysSoWithNoContent(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodDelete, "/Groups/7", "")

	require.Equal(t, http.StatusNoContent, recorder.Code)
	require.Equal(t, []int64{7}, directory.groups.deleted)
}

func TestAnUnknownGroupIsNotFound(t *testing.T) {
	directory := newRecordingDirectory()
	require.Equal(t, http.StatusNotFound, serve(t, directory, http.MethodGet, "/Groups/99", "").Code)
	require.Equal(t, http.StatusNotFound, serve(t, directory, http.MethodDelete, "/Groups/99", "").Code)
}

/* ── the filter, and the discovery documents ───────────────────────────── */

func TestAGroupFilterThisDirectoryCannotAnswerIsRefusedNotIgnored(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodGet, `/Groups?filter=members+eq+%2242%22`, "")

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	body := decodeBody(t, recorder)
	require.Equal(t, "invalidFilter", body["scimType"])
	// The message names what CAN be filtered on, so an operator can fix the
	// provider's configuration without reading this source.
	require.Contains(t, body["detail"], "displayName")
}

func TestAGroupListingCarriesTheFilterItWasGiven(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodGet,
		`/Groups?filter=displayName+eq+%22Platform+Team%22`, "")

	require.Equal(t, http.StatusOK, recorder.Code)
	expected, err := scimdirectory.ParseGroupFilter(`displayName eq "Platform Team"`)
	require.NoError(t, err)
	require.Equal(t, expected, directory.groups.filter)
}

func TestTheDiscoveryDocumentsAdvertiseGroupsBecauseGroupsAreServed(t *testing.T) {
	directory := newRecordingDirectory()

	types := decodeBody(t, serve(t, directory, http.MethodGet, "/ResourceTypes", ""))
	endpoints := map[string]bool{}
	for _, resource := range types["Resources"].([]any) {
		endpoints[resource.(map[string]any)["endpoint"].(string)] = true
	}
	require.True(t, endpoints["/Users"])
	require.True(t, endpoints["/Groups"],
		"a served resource that the catalogue omits is one no provider will ever call")

	schemas := decodeBody(t, serve(t, directory, http.MethodGet, "/Schemas", ""))
	ids := map[string]bool{}
	for _, resource := range schemas["Resources"].([]any) {
		ids[resource.(map[string]any)["id"].(string)] = true
	}
	require.True(t, ids[schemaGroup])
	require.True(t, ids[schemaProjectGrant])
}

/* ── an empty removal list is not "remove everybody" ───────────────────── */

// The difference between an OMITTED value and an EMPTY one is a group's whole
// membership. A client that computed a delta and found nothing to remove sends
// `"value": []`, and reading that as the attribute-wide removal empties the
// project behind a 200 nobody has a reason to look at.
func TestARemoveWithAnEmptyMemberListRemovesNobody(t *testing.T) {
	directory := newRecordingDirectory()
	directory.groups.bindings[7].members = []int{42, 43}

	recorder := serve(t, directory, http.MethodPatch, "/Groups/7",
		`{"Operations":[{"op":"remove","path":"members","value":[]}]}`)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Empty(t, directory.groups.replaced, "an empty list must not empty the group")
	require.Empty(t, directory.groups.removed)
	require.Equal(t, []int{42, 43}, directory.groups.bindings[7].members)
}

// An OMITTED value still means the attribute-wide removal RFC 7644 §3.5.2.2
// defines. A JSON null counts as omitted, which is what several clients send.
func TestARemoveWithNoMemberValueStillEmptiesTheGroup(t *testing.T) {
	for _, body := range []string{
		`{"Operations":[{"op":"remove","path":"members"}]}`,
		`{"Operations":[{"op":"remove","path":"members","value":null}]}`,
	} {
		directory := newRecordingDirectory()
		directory.groups.bindings[7].members = []int{42, 43}

		recorder := serve(t, directory, http.MethodPatch, "/Groups/7", body)

		require.Equal(t, http.StatusOK, recorder.Code, body)
		require.Equal(t, [][]int{nil}, directory.groups.replaced, body)
		require.Empty(t, directory.groups.bindings[7].members, body)
	}
}

/* ── a removal of somebody who does not exist is not a wedge ───────────── */

// A hard-deleted account must not stop a group from syncing. The removal names
// a membership that cannot exist, so skipping it reaches exactly the state the
// client asked for — and refusing means every later sync of that group fails on
// the same operation forever.
func TestARemovalOfADeletedAccountIsSkippedRatherThanRefused(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPatch, "/Groups/7", `{"Operations":[
		{"op":"add","path":"members","value":[{"value":"42"}]},
		{"op":"remove","path":"members[value eq \"nobody@corp.com\"]"}
	]}`)

	require.Equal(t, http.StatusOK, recorder.Code)
	// The add still landed, and the unresolvable removal was dropped rather
	// than taking the whole request down with it.
	require.Equal(t, [][]int{{42}}, directory.groups.added)
	require.Empty(t, directory.groups.removed)
}

// A GRANT still refuses an unresolvable member: applying the ones that resolved
// would report a membership this service only partly holds.
func TestAnUnresolvableMemberInAnAddStillRefusesTheWholeRequest(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPatch, "/Groups/7",
		`{"Operations":[{"op":"add","path":"members","value":[{"value":"42"},{"value":"nobody@corp.com"}]}]}`)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Empty(t, directory.groups.added)
	require.Empty(t, directory.groups.applied)
}

// An AMBIGUOUS value is refused on a removal too. Removing whichever of two
// accounts the database returns first is not a state anybody asked for.
func TestAnAmbiguousMemberValueIsRefusedOnARemovalAsWell(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPatch, "/Groups/7",
		`{"Operations":[{"op":"remove","path":"members","value":[{"value":"ambiguous"}]}]}`)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Empty(t, directory.groups.removed)
}

/* ── one PATCH reaches the store as one call ───────────────────────────── */

// The delta shape Entra ID sends is two operations in one request. They must
// arrive at the store together: applied through separate calls, a failure in
// the second commits the first and reports the whole PATCH as failed, so the
// group keeps somebody it was told to drop and nothing says so again.
func TestOnePatchIsAppliedAsOneCallSoItCannotLandByHalves(t *testing.T) {
	directory := newRecordingDirectory()
	directory.groups.bindings[7].members = []int{43}

	recorder := serve(t, directory, http.MethodPatch, "/Groups/7", `{"Operations":[
		{"op":"add","path":"members","value":[{"value":"42"}]},
		{"op":"remove","path":"members[value eq \"43\"]"}
	]}`)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Len(t, directory.groups.applied, 1, "one request, one store call")
	require.Equal(t, []scimdirectory.GroupOperation{
		{Kind: scimdirectory.GroupAddMembers, Members: []int{42}},
		{Kind: scimdirectory.GroupRemoveMembers, Members: []int{43}},
	}, directory.groups.applied[0])
}

/* ── a create applies the membership before it stamps the identifier ───── */

// The two writes can fail independently, and the order decides what a failed
// push leaves behind. Stamping first leaves an identifier the operator never
// saw applied on a push the provider recorded as failed.
func TestACreateAppliesTheMembershipBeforeItStampsTheIdentifier(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPost, "/Groups",
		`{"displayName":"Platform Team","externalId":"grp-1","members":[{"value":"42"}]}`)

	require.Equal(t, http.StatusCreated, recorder.Code)
	require.Equal(t, []string{"members", "adopt"}, directory.groups.order)
	// And the resource returned carries the identifier that was just stamped.
	require.Equal(t, "grp-1", decodeBody(t, recorder)["externalId"])
}

/* ── Entra group shapes ────────────────────────────────────────────────── */

func TestGroupExternalIDReplaceIsApplied(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPatch, "/Groups/7",
		`{"Operations":[{"op":"Replace","path":"externalId","value":"grp-2"}]}`)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []string{"grp-2"}, directory.groups.externalIDs)
	require.Len(t, directory.groups.applied, 1)
}

func TestPathlessMembersAddAndReplaceAreReadWithTheirVerb(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPatch, "/Groups/7",
		`{"Operations":[{"op":"Add","value":{"members":[{"value":"42"}],"externalId":"grp-3"}}]}`)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, [][]int{{42}}, directory.groups.added)
	require.Equal(t, []string{"grp-3"}, directory.groups.externalIDs)

	recorder = serve(t, directory, http.MethodPatch, "/Groups/7",
		`{"Operations":[{"op":"Replace","value":{"members":[{"value":"43"}]}}]}`)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, [][]int{{43}}, directory.groups.replaced)
}

// A path-less REMOVE used to be read with the add/replace logic, so removing
// the externalId SET it. It is refused, like the users one, and applies nothing.
func TestPathlessGroupRemoveIsRefusedAndAppliesNothing(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPatch, "/Groups/7", `{"Operations":[
		{"op":"Add","path":"members","value":[{"value":"43"}]},
		{"op":"Remove","value":{"externalId":"grp-9","displayName":"Renamed"}}]}`)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, "noTarget", decodeBody(t, recorder)["scimType"])
	require.Empty(t, directory.groups.externalIDs)
	require.Empty(t, directory.groups.applied)
}

func TestEntraMemberAddThenBracketedRemoveInOneRequest(t *testing.T) {
	directory := newRecordingDirectory()
	recorder := serve(t, directory, http.MethodPatch, "/Groups/7", `{"Operations":[
		{"op":"Add","path":"members","value":[{"value":"43"}]},
		{"op":"Remove","path":"members[value eq \"42\"]"}]}`)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Len(t, directory.groups.applied, 1, "one request, one store call")
	require.Len(t, directory.groups.applied[0], 2)
}

func TestExcludedAttributesMembersIsHonouredOnGroups(t *testing.T) {
	directory := newRecordingDirectory()
	directory.groups.bindings[7].members = []int{42}

	body := decodeBody(t, serve(t, directory, http.MethodGet, "/Groups/7?excludedAttributes=members", ""))
	require.NotContains(t, body, "members")
	require.Equal(t, "Platform Team", body["displayName"])

	body = decodeBody(t, serve(t, directory, http.MethodGet, "/Groups/7", ""))
	require.Contains(t, body, "members")

	list := decodeBody(t, serve(t, directory, http.MethodGet, "/Groups?excludedAttributes=members", ""))
	for _, resource := range list["Resources"].([]any) {
		require.NotContains(t, resource.(map[string]any), "members")
	}
	body = decodeBody(t, serve(t, directory, http.MethodGet, "/Groups/7?attributes=displayName", ""))
	require.Contains(t, body, "displayName")
	require.NotContains(t, body, "members")
}

// The unbound-group refusal stays, and its body is a proper SCIM error.
func TestUnboundGroupRefusalIsAScimErrorBody(t *testing.T) {
	recorder := serve(t, newRecordingDirectory(), http.MethodPost, "/Groups", `{"displayName":"Finance"}`)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, contentType, recorder.Header().Get("Content-Type"))
	body := decodeBody(t, recorder)
	require.Equal(t, []any{schemaError}, body["schemas"])
	require.Equal(t, "400", body["status"])
	require.Equal(t, "invalidValue", body["scimType"])
	require.NotEmpty(t, body["detail"])
}
