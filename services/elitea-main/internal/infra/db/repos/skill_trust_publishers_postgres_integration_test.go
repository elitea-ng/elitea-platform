package repos

// An administrator can trust a publisher project (ADR-0051 amendment 1, A1.6,
// the option the amendment left for later; skills epic 2169 follow-up 2184):
// every reader of the trust state must agree, against a REAL migrated database.
// It SKIPS without ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

func TestATrustedPublisherMakesItsSkillsTrustedEverywhere(t *testing.T) {
	pool := newSkillsTestPool(t)
	applySkillTrustMigration(t, pool)
	ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "7", UserID: "7"}), 60*time.Second)
	defer cancel()
	seedListedSkill(t, pool, 1200, "From the library", "Library steps", `{"parent_project_id": 2}`)
	seedListedSkill(t, pool, 1201, "From a stranger", "Stranger steps", `{"parent_project_id": 3}`)
	seedListedSkill(t, pool, 1202, "Unknown lineage", "Odd steps", `{"parent_project_id": "abc"}`)

	untrusted := func() map[int64]bool {
		skillsRead, err := NewProjectSkillsRepo(pool).ResolveCurrentProjectSkills(ctx, 1, []int64{1200, 1201, 1202})
		if err != nil {
			t.Fatalf("ResolveCurrentProjectSkills: %v", err)
		}
		out := map[int64]bool{}
		for _, skill := range skillsRead {
			out[skill.SkillID] = skill.Untrusted
		}
		return out
	}
	state := func(id int64) (listState string, routeState string, publisherTrusted bool) {
		list, err := NewSkillsRepo(pool).List(ctx, "1", skills.ListParams{Page: 1, PageSize: 50})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, item := range list.Items {
			if item.Trust != nil && item.ID == strconv.FormatInt(id, 10) {
				listState = item.Trust.State
			}
		}
		status, err := NewSkillTrustRepo(pool).Get(ctx, "1", id)
		if err != nil {
			t.Fatalf("trust route Get: %v", err)
		}
		return listState, status.State, status.PublisherTrusted
	}

	// Nobody is trusted yet: every foreign skill is untrusted, in every reader.
	if got := untrusted(); !got[1200] || !got[1201] || !got[1202] {
		t.Fatalf("before the setting: untrusted = %v, want all untrusted", got)
	}

	// An administrator trusts project 2.
	if _, err := pool.Exec(context.Background(), `INSERT INTO centry.platform_config (section, key, value) VALUES ($1, $2, '[2]'::jsonb)`,
		platformconfig.SectionSkillPublishing, platformconfig.KeyTrustedSkillPublisherProjectIDs); err != nil {
		t.Fatalf("write the setting: %v", err)
	}
	if got := untrusted(); got[1200] || !got[1201] || !got[1202] {
		t.Fatalf("with project 2 trusted: untrusted = %v, want only 1201 and 1202", got)
	}
	if list, route, byPublisher := state(1200); list != "trusted" || route != "trusted" || !byPublisher {
		t.Fatalf("a trusted publisher's skill: list=%q route=%q publisher_trusted=%v", list, route, byPublisher)
	}
	if list, route, byPublisher := state(1201); list != "untrusted" || route != "untrusted" || byPublisher {
		t.Fatalf("another publisher's skill: list=%q route=%q publisher_trusted=%v", list, route, byPublisher)
	}
	// A lineage that names no publisher is never trusted through one.
	if list, route, _ := state(1202); list != "untrusted" || route != "untrusted" {
		t.Fatalf("an unknown lineage: list=%q route=%q, want untrusted", list, route)
	}

	// Removing the publisher puts the skill back to quoted data.
	if _, err := pool.Exec(context.Background(), `UPDATE centry.platform_config SET value = '[]'::jsonb WHERE section = $1 AND key = $2`,
		platformconfig.SectionSkillPublishing, platformconfig.KeyTrustedSkillPublisherProjectIDs); err != nil {
		t.Fatal(err)
	}
	if got := untrusted(); !got[1200] {
		t.Fatalf("after removing the publisher: untrusted = %v, want 1200 untrusted again", got)
	}
}
