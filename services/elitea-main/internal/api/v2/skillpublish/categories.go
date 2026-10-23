package skillpublish

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// defaultSkillCategories is the predefined list the reference seeds skills with
// (legacy/plugins/elitea_core/utils/constants.py DEFAULT_SKILL_CATEGORIES). It
// is currently identical to the agent list, but it is a SEPARATE constant on
// purpose: the reference manages the two independently, and aliasing them here
// would make a future divergence a silent behaviour change on the agent side.
var defaultSkillCategories = []string{
	"Business Analyst",
	"Quality Assurance",
	"Development",
	"DevOps",
	"Project Management",
	"Knowledge & Documentation",
	"Elitea",
	"Epam",
	"Other",
}

// fallbackCategory is the permanent bucket an uncategorised publish lands in.
// The catalog's category filter matches a real tag, so a publish with no
// category must still carry one or it is unreachable through the filter.
const fallbackCategory = "Other"

// activeCategories is the list a publish is validated against: the built-in
// defaults, then whatever the admin Features page added, with the fallback
// forced last.
//
// The extras used to be absent — the package doc recorded them as deliberately
// not ported, because the Go admin config schema had no section to author them
// in and "reading a key no page can write" is a lookup that can only miss. The
// section exists now (`skill_publishing` in internal/api/v2/admin), so the read
// has a writer and the deployment's tenth category is reachable.
//
// Three rules, all of them the reference's (skill_category_utils.py):
//
//   - deduplication is case-insensitive and the first-seen spelling wins, so an
//     operator who re-types "development" does not create a second bucket that
//     the filter shows separately from the built-in one;
//   - "Other" is always last, even if an extra re-adds it, because it is the
//     fallback and reading it in the middle of the list implies it is a choice
//     like the others;
//   - an unreadable config store yields the defaults rather than an error. The
//     permissive answer here is the built-in list: a database hiccup must not
//     make every publish fail category validation.
func (h *Handler) activeCategories(ctx context.Context) []string {
	return mergeCategories(defaultSkillCategories, h.extraCategories(ctx))
}

// mergeCategories is the ordering rule on its own, so it can be tested without
// a database.
func mergeCategories(defaults, extras []string) []string {
	result := make([]string, 0, len(defaults)+len(extras))
	seen := make(map[string]bool, len(defaults)+len(extras))
	for _, name := range append(append([]string{}, defaults...), extras...) {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" || strings.EqualFold(trimmed, fallbackCategory) {
			continue
		}
		if key := strings.ToLower(trimmed); !seen[key] {
			seen[key] = true
			result = append(result, trimmed)
		}
	}
	return append(result, fallbackCategory)
}

// extraCategories reads the operator-authored additions.
func (h *Handler) extraCategories(ctx context.Context) []string {
	values, err := platformconfig.Load(ctx, h.pool, platformconfig.SectionSkillPublishing)
	if err != nil {
		return nil
	}
	return values.Strings(platformconfig.KeySkillCategories)
}

// isDefaultCategory reports whether a name is one of the built-in defaults, so
// the listing can mark the operator's own additions as removable.
func isDefaultCategory(name string) bool {
	for _, builtin := range defaultSkillCategories {
		if strings.EqualFold(builtin, name) {
			return true
		}
	}
	return false
}

// resolveCategory returns the canonical spelling of a category from the given
// active list, or "" when the name is not one. Matching is case-insensitive, as
// in the reference.
//
// It takes the active list rather than reading it, because both call sites
// (publish and validate) already resolved it once for the same request and a
// second read would let one request judge a category against two different
// lists if an operator saved between them.
func resolveCategory(active []string, name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return ""
	}
	for _, candidate := range active {
		if strings.EqualFold(candidate, trimmed) {
			return candidate
		}
	}
	return ""
}

// applyCategoryToTags returns the tag list a published snapshot should carry:
// every category-looking tag replaced by exactly one canonical category.
//
// When the caller names no category, an existing category tag on the version is
// kept and only a version with none gets the fallback — the same precedence the
// reference applies, and the reason an already-categorised republish does not
// silently drop into "Other".
func applyCategoryToTags(active []string, tags []string, category string) []string {
	resolved := resolveCategory(active, category)
	if resolved == "" {
		for _, tag := range tags {
			if resolveCategory(active, tag) != "" {
				return tags
			}
		}
		resolved = fallbackCategory
	}

	result := make([]string, 0, len(tags)+1)
	for _, tag := range tags {
		if resolveCategory(active, tag) == "" {
			result = append(result, tag)
		}
	}
	return append(result, resolved)
}

// SkillCategories serves the active skill category list: the built-in defaults
// plus the admin Features page's additions.
//
// Legacy: skill_categories.py.
func (h *Handler) SkillCategories(w http.ResponseWriter, r *http.Request) {
	if _, ok := projectSchema(chi.URLParam(r, "projectID")); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return
	}
	active := h.activeCategories(r.Context())
	categories := make([]map[string]any, 0, len(active))
	for _, name := range active {
		categories = append(categories, map[string]any{"name": name, "is_default": isDefaultCategory(name)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": categories, "total": len(categories)})
}
