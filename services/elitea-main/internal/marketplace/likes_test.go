package marketplace

import (
	"strings"
	"testing"
)

// Skills epic 2169, S6: Most liked is a sort key for both kinds, and a like is
// counted under the entity name of the listing's kind.
func TestLikesSortIsAcceptedAndOrdersByTheLikeCount(t *testing.T) {
	p := SearchParams{Sort: SortLikes}
	if err := p.Validate(); err != nil || p.Sort != SortLikes {
		t.Fatalf("Validate() = %v, sort %q", err, p.Sort)
	}
	order := orderBy(p, &argList{})
	if !strings.HasPrefix(order, " ORDER BY likes DESC,") {
		t.Fatalf("order = %q", order)
	}
}

func TestLikeExpressionsReadTheEntityNameOfTheListingKind(t *testing.T) {
	args := &argList{}
	likes, liked := likeExpressions("p_1", 7, args)
	for _, want := range []string{"p_1.social_likes", "WHEN 'skill' THEN 'skill' ELSE 'application'", "sl.entity_id = l.entity_id"} {
		if !strings.Contains(likes, want) {
			t.Errorf("likes expression lacks %q: %s", want, likes)
		}
	}
	if !strings.Contains(liked, "sm.user_id = $1") || len(args.values) != 1 || args.values[0] != int64(7) {
		t.Errorf("liked = %s, args = %v: the caller must be a bound parameter", liked, args.values)
	}

	// An anonymous caller has liked nothing and binds nothing.
	anonymous := &argList{}
	if _, liked := likeExpressions("p_1", 0, anonymous); liked != "false" || len(anonymous.values) != 0 {
		t.Errorf("anonymous liked = %q args = %v", liked, anonymous.values)
	}
}
