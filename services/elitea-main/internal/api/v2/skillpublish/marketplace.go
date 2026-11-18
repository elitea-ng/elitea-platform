package skillpublish

import (
	"context"
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
)

// listingWrite names what one skill publish records in the Marketplace
// (ADR-0051 decision 2).
type listingWrite struct {
	// schema is the quoted public schema, where the catalogue version lives.
	schema string
	// sourceProject and sourceSkill are where the skill was authored; for an
	// in-place publish in the public project they are the public ones.
	sourceProject, sourceSkill int64
	// twinSkill and publicVersion are the catalogue skill and version.
	twinSkill, publicVersion  int64
	versionName, releaseNotes string
	publisher                 int64
	activeCategories          []string
	category                  string
}

// categoryOf is the category a published skill's tags carry: the one tag that
// names an active category.
func categoryOf(active, tags []string) string {
	for _, tag := range tags {
		if resolved := resolveCategory(active, tag); resolved != "" {
			return resolved
		}
	}
	return ""
}

// recordSkillListing writes the listing and the listing version of a skill
// publish, in the publish transaction. A skill has no tool or credential, so
// its snapshot is its instructions and tags.
func recordSkillListing(ctx context.Context, db queryExecer, w listingWrite) error {
	snap, err := marketplace.BuildSkillSnapshot(ctx, db, marketplace.SkillSource{
		Schema: w.schema, SkillID: w.twinSkill, VersionID: w.publicVersion,
	})
	if err != nil {
		return fmt.Errorf("snapshot the skill: %w", err)
	}
	_, err = marketplace.RecordPublish(ctx, db, marketplace.PublishRecord{
		Snapshot: snap, SourceProjectID: w.sourceProject, SourceEntityID: w.sourceSkill,
		EntityID: w.twinSkill, VersionID: w.publicVersion, VersionName: w.versionName,
		ReleaseNotes: w.releaseNotes, PublisherID: w.publisher, Category: categoryOf(w.activeCategories, snap.Tags),
	})
	return err
}
