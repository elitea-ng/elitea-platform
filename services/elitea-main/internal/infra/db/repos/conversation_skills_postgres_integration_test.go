package repos

// Conversation skills (tenant 0251; ADR-0050 amendment 1, A1.2; skills epic 2169
// story S3) against a REAL migrated database. It SKIPS without
// ELITEA_TEST_DATABASE_URL.
//
// Each case asserts rows or an answer that a missing CHECK, a missing row lock, a
// missing cascade or a pin that carried text could not have produced.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversationskills"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/skilltrust"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

const pinProject = "1"

type pinFixture struct {
	pool *pgxpool.Pool
	repo *ConversationSkillsRepo
	ctx  context.Context
	// conversation is the numeric id, uuid its uuid form.
	conversation string
	uuid         string
}

func newPinFixture(t *testing.T) pinFixture {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "7", UserID: "7"}), 60*time.Second)
	t.Cleanup(cancel)
	var id int
	var uuid string
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
VALUES (gen_random_uuid(), 'pins', 7, 'agent', now() AT TIME ZONE 'UTC') RETURNING id, uuid::text`).Scan(&id, &uuid); err != nil {
		t.Fatalf("seed a conversation: %v", err)
	}
	return pinFixture{pool: pool, repo: NewConversationSkillsRepo(pool), ctx: ctx, conversation: strconv.Itoa(id), uuid: uuid}
}

var seededListings int64

func seedListing(t *testing.T, pool *pgxpool.Pool, name, instructions, listingState, versionState string) (listingID, versionID int64) {
	t.Helper()
	ctx := context.Background()
	seededListings++
	if err := pool.QueryRow(ctx, `
INSERT INTO elitea_marketplace.listings (kind, source_project_id, source_entity_id, entity_id, name, description, state)
VALUES ('skill', 2, $1, $1, $2, 'listing description', $3) RETURNING id`,
		500+seededListings, name, listingState).Scan(&listingID); err != nil {
		t.Fatalf("seed a listing: %v", err)
	}
	versionID = 4242
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_marketplace.listing_versions (listing_id, version_id, name, digest, definition, state)
VALUES ($1, $2, 'v1', decode(repeat('ab', 32), 'hex'), jsonb_build_object('name', $3::text, 'description', 'listing description', 'instructions', $4::text), $5)`,
		listingID, versionID, name, instructions, versionState); err != nil {
		t.Fatalf("seed a listing version: %v", err)
	}
	return listingID, versionID
}

func TestPinningASkillStoresARowAndNoText(t *testing.T) {
	f := newPinFixture(t)
	seedTrustSkill(t, f.pool, 901, "Triage", "Triage steps", `{}`)

	pin, created, err := f.repo.PinSkill(f.ctx, pinProject, f.conversation, 901)
	if err != nil || !created {
		t.Fatalf("PinSkill: created=%v err=%v", created, err)
	}
	if pin.Kind != conversationskills.KindProject || pin.SkillID == nil || *pin.SkillID != 901 || pin.Name != "Triage" ||
		pin.PinnedBy != 7 || pin.Trust.State != "trusted" {
		t.Fatalf("pin = %+v", pin)
	}
	// The uuid form of the conversation addresses the same row, and pinning again answers the pin.
	again, created, err := f.repo.PinSkill(f.ctx, pinProject, f.uuid, 901)
	if err != nil || created || again.ID != pin.ID {
		t.Fatalf("pinning twice: created=%v id=%d want %d err=%v", created, again.ID, pin.ID, err)
	}
	var rows int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.conversation_skills`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows = %d err=%v", rows, err)
	}
	// The row holds ids, never the skill text: an edit of the skill reaches the next turn.
	var columns int
	if err := f.pool.QueryRow(context.Background(), `
SELECT count(*) FROM information_schema.columns WHERE table_schema = 'p_1' AND table_name = 'conversation_skills' AND column_name IN ('instructions', 'content', 'text')`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("a pin must not carry text: columns=%d err=%v", columns, err)
	}
}

func TestPinningRefusesWhatTheCallerCannotUse(t *testing.T) {
	f := newPinFixture(t)
	seedTrustSkill(t, f.pool, 902, "Hidden", "x", `{}`)

	for name, call := range map[string]func() error{
		"a skill that does not exist": func() error { _, _, err := f.repo.PinSkill(f.ctx, pinProject, f.conversation, 99999); return err },
		"a conversation that does not exist": func() error {
			_, _, err := f.repo.PinSkill(f.ctx, pinProject, "999999", 902)
			return err
		},
		"a listing that does not exist": func() error { _, _, err := f.repo.PinListing(f.ctx, pinProject, f.conversation, 77, 78); return err },
	} {
		var apiErr *apierr.APIError
		if err := call(); !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
			t.Errorf("%s: error = %v, want a 404", name, err)
		}
	}
	// Anonymous callers pin nothing.
	if _, _, err := f.repo.PinSkill(context.Background(), pinProject, f.conversation, 902); err == nil {
		t.Fatal("pinning without a caller must fail")
	}
}

func TestAConversationHoldsAtMostFiveSkills(t *testing.T) {
	f := newPinFixture(t)
	for id := 910; id < 916; id++ {
		seedTrustSkill(t, f.pool, id, "Skill "+strconv.Itoa(id), "steps", `{}`)
	}
	for id := 910; id < 915; id++ {
		if _, _, err := f.repo.PinSkill(f.ctx, pinProject, f.conversation, int64(id)); err != nil {
			t.Fatalf("pin %d: %v", id, err)
		}
	}
	_, _, err := f.repo.PinSkill(f.ctx, pinProject, f.conversation, 915)
	var apiErr *apierr.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Fatalf("a sixth pin: error = %v, want 409", err)
	}
	// Pinning one of the five again is not a sixth.
	if _, created, err := f.repo.PinSkill(f.ctx, pinProject, f.conversation, 910); err != nil || created {
		t.Fatalf("an existing pin: created=%v err=%v", created, err)
	}
	var rows int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.conversation_skills`).Scan(&rows); err != nil || rows != 5 {
		t.Fatalf("rows = %d err=%v", rows, err)
	}
}

func TestAPinnedForkedSkillShowsItsTrust(t *testing.T) {
	f := newPinFixture(t)
	const text = "Forked steps"
	seedTrustSkill(t, f.pool, 920, "Forked", text, `{"parent_project_id": 2}`)
	pin, _, err := f.repo.PinSkill(f.ctx, pinProject, f.conversation, 920)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Trust.State != "untrusted" || pin.Trust.PublisherProjectID == nil || *pin.Trust.PublisherProjectID != 2 {
		t.Fatalf("a fresh fork pins untrusted: %+v", pin.Trust)
	}
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO p_1.skill_trust (skill_id, revision, trusted_by) VALUES (920, $1, 7)`, skilltrust.Revision(text)); err != nil {
		t.Fatal(err)
	}
	pins, err := f.repo.List(f.ctx, pinProject, f.conversation)
	if err != nil || len(pins) != 1 || pins[0].Trust.State != "trusted" {
		t.Fatalf("after trusting the revision: pins=%+v err=%v", pins, err)
	}
}

func TestListingPinsAreReadByReferenceAndRefuseWhenYanked(t *testing.T) {
	f := newPinFixture(t)
	listing, version := seedListing(t, f.pool, "Reviewer", "Review steps", "listed", "available")

	pin, created, err := f.repo.PinListing(f.ctx, pinProject, f.conversation, listing, version)
	if err != nil || !created || pin.Kind != conversationskills.KindListing || pin.Name != "Reviewer" ||
		pin.VersionName != "v1" || pin.Trust.State != "untrusted" || pin.SkillID != nil {
		t.Fatalf("listing pin = %+v created=%v err=%v", pin, created, err)
	}
	// Nothing was forked into the project.
	var skills int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.skills`).Scan(&skills); err != nil || skills != 0 {
		t.Fatalf("a listing pin forked %d skills (err=%v)", skills, err)
	}
	// The turn reads the text from the listing version.
	resolved, err := f.repo.ResolveConversationSkills(f.ctx, 1, f.uuid)
	if err != nil || len(resolved.Listing) != 1 || resolved.Listing[0].Instructions != "Review steps" ||
		resolved.Listing[0].ListingID != listing || resolved.Listing[0].VersionID != version {
		t.Fatalf("resolved = %+v err=%v", resolved, err)
	}
	// Yank the version: the next turn is refused with the answer a missing skill gets.
	if _, err := f.pool.Exec(context.Background(), `UPDATE elitea_marketplace.listing_versions SET state = 'yanked' WHERE listing_id = $1`, listing); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.ResolveConversationSkills(f.ctx, 1, f.uuid); !errors.Is(err, agentexecutionapp.ErrProjectSkillUnknown) {
		t.Fatalf("a yanked pin: error = %v", err)
	}
	// The pin still lists, so the person can remove it.
	if pins, err := f.repo.List(f.ctx, pinProject, f.conversation); err != nil || len(pins) != 1 {
		t.Fatalf("list after yank: %+v err=%v", pins, err)
	}
	// A yanked, withdrawn or delisted listing cannot be pinned.
	yankedListing, yankedVersion := seedListing(t, f.pool, "Yanked", "x", "listed", "yanked")
	delisted, delistedVersion := seedListing(t, f.pool, "Delisted", "x", "delisted", "available")
	for name, ids := range map[string][2]int64{"a yanked version": {yankedListing, yankedVersion}, "a delisted listing": {delisted, delistedVersion}} {
		var apiErr *apierr.APIError
		if _, _, err := f.repo.PinListing(f.ctx, pinProject, f.conversation, ids[0], ids[1]); !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
			t.Errorf("%s: error = %v, want a 404", name, err)
		}
	}
}

func TestUnpinningAndTheCascades(t *testing.T) {
	f := newPinFixture(t)
	seedTrustSkill(t, f.pool, 930, "One", "steps", `{}`)
	seedTrustSkill(t, f.pool, 931, "Two", "steps", `{}`)
	one, _, err := f.repo.PinSkill(f.ctx, pinProject, f.conversation, 930)
	if err != nil {
		t.Fatal(err)
	}
	two, _, err := f.repo.PinSkill(f.ctx, pinProject, f.conversation, 931)
	if err != nil {
		t.Fatal(err)
	}
	// A pin of another conversation cannot be removed through this one.
	var other string
	if err := f.pool.QueryRow(context.Background(), `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
VALUES (gen_random_uuid(), 'other', 7, 'agent', now() AT TIME ZONE 'UTC') RETURNING id::text`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	var apiErr *apierr.APIError
	if err := f.repo.Unpin(f.ctx, pinProject, other, one.ID); !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("unpin through another conversation: error = %v", err)
	}
	if err := f.repo.Unpin(f.ctx, pinProject, f.conversation, one.ID); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	if err := f.repo.Unpin(f.ctx, pinProject, f.conversation, one.ID); !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("unpin twice: error = %v", err)
	}
	// Deleting the skill takes its pin; deleting the conversation takes the rest.
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM p_1.skills WHERE id = 931`); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.conversation_skills WHERE id = $1`, two.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("a deleted skill left %d pins (err=%v)", rows, err)
	}
	if _, _, err := f.repo.PinSkill(f.ctx, pinProject, f.conversation, 930); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM p_1.chat_conversations WHERE id = $1::int`, f.conversation); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.conversation_skills`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("a deleted conversation left %d pins (err=%v)", rows, err)
	}
}

func TestAPinHoldsExactlyOneForm(t *testing.T) {
	f := newPinFixture(t)
	seedTrustSkill(t, f.pool, 940, "Both", "steps", `{}`)
	for name, statement := range map[string]string{
		"a skill and a listing":       `INSERT INTO p_1.conversation_skills (conversation_id, skill_id, listing_id, listing_version_id, pinned_by) VALUES ($1::int, 940, 1, 1, 7)`,
		"neither":                     `INSERT INTO p_1.conversation_skills (conversation_id, pinned_by) VALUES ($1::int, 7)`,
		"a listing without a version": `INSERT INTO p_1.conversation_skills (conversation_id, listing_id, pinned_by) VALUES ($1::int, 1, 7)`,
	} {
		if _, err := f.pool.Exec(context.Background(), statement, f.conversation); err == nil {
			t.Errorf("%s must be refused by the table", name)
		}
	}
}

func TestResolveConversationSkillsReadsPinsInOrderForTheTurn(t *testing.T) {
	f := newPinFixture(t)
	seedTrustSkill(t, f.pool, 950, "First", "steps", `{}`)
	seedTrustSkill(t, f.pool, 951, "Second", "steps", `{}`)
	for _, id := range []int64{951, 950} {
		if _, _, err := f.repo.PinSkill(f.ctx, pinProject, f.conversation, id); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.repo.ResolveConversationSkills(f.ctx, 1, f.uuid)
	if err != nil || len(got.ProjectSkillIDs) != 2 || got.ProjectSkillIDs[0] != 951 || got.ProjectSkillIDs[1] != 950 || len(got.Listing) != 0 {
		t.Fatalf("resolved = %+v err=%v (oldest pin first)", got, err)
	}
	// A conversation without pins, or an unknown one, answers nothing.
	empty, err := f.repo.ResolveConversationSkills(f.ctx, 1, "00000000-0000-4000-8000-000000000999")
	if err != nil || len(empty.ProjectSkillIDs) != 0 || len(empty.Listing) != 0 {
		t.Fatalf("an unknown conversation: %+v err=%v", empty, err)
	}
}
