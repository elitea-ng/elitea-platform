package marketplace

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Sort keys of the search.
const (
	SortRelevance = "relevance"
	SortHires     = "hires"
	SortLikes     = "likes"
	SortRating    = "rating"
	SortNewest    = "newest"
	SortName      = "name"
)

// Publisher is the person who published a listing.
type Publisher struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// VersionRef names one version.
type VersionRef struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	PublishedAt time.Time `json:"published_at"`
}

// Hired is one hire of a listing in the project that asks.
type Hired struct {
	ApplicationID string `json:"application_id"`
	Mode          string `json:"mode"`
	VersionID     string `json:"version_id"`
	VersionName   string `json:"version_name"`
}

// ListingSummary is one listing as a card shows it.
type ListingSummary struct {
	ID            string      `json:"id"`
	Kind          string      `json:"kind"`
	EntityID      string      `json:"entity_id"`
	Name          string      `json:"name"`
	Description   string      `json:"description"`
	IconURL       *string     `json:"icon_url"`
	Category      string      `json:"category"`
	Tags          []string    `json:"tags"`
	Publisher     *Publisher  `json:"publisher"`
	Tier          string      `json:"tier"`
	CurrentVer    *VersionRef `json:"current_version"`
	Hires         int         `json:"hires"`
	RatingAvg     *float64    `json:"rating_avg"`
	RatingCount   int         `json:"rating_count"`
	Likes         int         `json:"likes"`
	IsLiked       bool        `json:"is_liked"`
	Featured      bool        `json:"featured"`
	RequiredTypes []string    `json:"required_types"`
	HasWriteTools bool        `json:"has_write_tools"`
	State         string      `json:"state"`
	Hired         []Hired     `json:"hired"`
}

// FacetCount is one facet value and its count.
type FacetCount struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// Facets are the counts behind the Browse sidebar.
type Facets struct {
	Kinds         []FacetCount `json:"kinds"`
	Categories    []FacetCount `json:"categories"`
	Tags          []FacetCount `json:"tags"`
	Publishers    []FacetCount `json:"publishers"`
	Tiers         []FacetCount `json:"tiers"`
	RequiredTypes []FacetCount `json:"required_types"`
	HiredHere     int          `json:"hired_here"`
}

// Page is a page of listings with the facet counts.
type Page struct {
	Rows   []ListingSummary `json:"rows"`
	Total  int              `json:"total"`
	Facets Facets           `json:"facets"`
}

// SearchParams is a validated search.
type SearchParams struct {
	// ProjectID is the project that hires. UserID is the caller, for the
	// liked flag. PublicSchema is the quoted schema of the public project,
	// which holds the likes.
	ProjectID    int64
	UserID       int64
	PublicSchema string

	Kind         string
	Query        string
	Categories   []string
	Tags         []string
	Requires     []string
	PublisherID  int64
	EntityID     int64
	MaxTier      string
	MinRating    float64
	HiredHere    bool
	FeaturedOnly bool
	Sort         string
	Limit        int
	Offset       int
}

// ParamError names the parameter a caller got wrong.
type ParamError struct{ Param, Message string }

func (e *ParamError) Error() string { return e.Param + ": " + e.Message }

// Validate checks the ranges and fills the defaults.
func (p *SearchParams) Validate() error {
	switch p.Kind {
	case "", KindAgent, KindSkill, KindTeam:
	default:
		return &ParamError{"kind", "must be agent, skill or team"}
	}
	switch p.Sort {
	case "":
		p.Sort = SortRelevance
	case SortRelevance, SortHires, SortLikes, SortRating, SortNewest, SortName:
	default:
		return &ParamError{"sort", "must be relevance, hires, likes, rating, newest or name"}
	}
	if p.MaxTier != "" && TierRank(p.MaxTier) < 0 {
		return &ParamError{"max_tier", "must be low, medium, high or critical"}
	}
	if p.MinRating < 0 || p.MinRating > 5 {
		return &ParamError{"min_rating", "must be from 0 to 5"}
	}
	if p.Limit <= 0 {
		p.Limit = 24
	}
	if p.Limit > 100 {
		return &ParamError{"limit", "must be at most 100"}
	}
	if p.Offset < 0 {
		return &ParamError{"offset", "must not be negative"}
	}
	if len([]rune(p.Query)) > 200 {
		return &ParamError{"query", "must be at most 200 characters"}
	}
	return nil
}

type argList struct{ values []any }

func (a *argList) add(v any) string {
	a.values = append(a.values, v)
	return "$" + strconv.Itoa(len(a.values))
}

// likeEscape escapes the characters that LIKE reads as pattern.
func likeEscape(word string) string {
	word = strings.ReplaceAll(word, `\`, `\\`)
	word = strings.ReplaceAll(word, `%`, `\%`)
	return strings.ReplaceAll(word, `_`, `\_`)
}

// facet names the groups a count leaves its own filter out of.
const (
	facetNone      = ""
	facetKind      = "kind"
	facetCategory  = "category"
	facetTag       = "tag"
	facetPublisher = "publisher"
	facetTier      = "tier"
	facetRequires  = "requires"
)

// where builds the predicates of a search, every value a bind parameter. A
// facet group leaves out its own filter so its counts show what a click would
// add.
func (p SearchParams) where(args *argList, exclude string) string {
	conds := []string{"l.state = 'listed'",
		"EXISTS (SELECT 1 FROM elitea_marketplace.listing_versions hv WHERE hv.listing_id = l.id AND hv.withdrawn_at IS NULL AND hv.state <> 'yanked')"}
	if p.Kind != "" && exclude != facetKind {
		conds = append(conds, "l.kind = "+args.add(p.Kind))
	}
	if p.EntityID > 0 {
		conds = append(conds, "l.entity_id = "+args.add(p.EntityID))
	}
	for _, word := range strings.Fields(strings.ToLower(p.Query)) {
		conds = append(conds, "l.search_text LIKE "+args.add("%"+likeEscape(word)+"%")+` ESCAPE '\'`)
	}
	if len(p.Categories) > 0 && exclude != facetCategory {
		conds = append(conds, "l.category = ANY("+args.add(p.Categories)+"::text[])")
	}
	if len(p.Tags) > 0 && exclude != facetTag {
		conds = append(conds, "l.tags && "+args.add(p.Tags)+"::text[]")
	}
	if len(p.Requires) > 0 && exclude != facetRequires {
		conds = append(conds, "l.required_types && "+args.add(p.Requires)+"::text[]")
	}
	if p.PublisherID > 0 && exclude != facetPublisher {
		conds = append(conds, "l.publisher_id = "+args.add(p.PublisherID))
	}
	if p.MaxTier != "" && exclude != facetTier {
		allowed := []string{}
		for _, tier := range Tiers {
			allowed = append(allowed, tier)
			if tier == p.MaxTier {
				break
			}
		}
		conds = append(conds, "l.risk_tier = ANY("+args.add(allowed)+"::text[])")
	}
	if p.MinRating > 0 {
		conds = append(conds, "l.rating_avg >= "+args.add(p.MinRating))
	}
	if p.HiredHere {
		conds = append(conds, "EXISTS (SELECT 1 FROM elitea_marketplace.installs hi WHERE hi.listing_id = l.id AND hi.dismissed_at IS NULL AND hi.project_id = "+args.add(p.ProjectID)+")")
	}
	if p.FeaturedOnly {
		conds = append(conds, "l.featured")
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

const currentVersionSQL = `(SELECT cv.version_id || '|' || cv.name || '|' || to_char(cv.published_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
    FROM elitea_marketplace.listing_versions cv
    WHERE cv.listing_id = l.id AND cv.withdrawn_at IS NULL AND cv.state <> 'yanked'
    ORDER BY cv.published_at DESC, cv.version_id DESC LIMIT 1)`

// likeExpressions are the select-list expressions of a listing's like count
// and of whether the caller liked it. Agents are liked as the `application`
// entity and skills as the `skill` entity, both by the listing's public twin id
// (`entity_id`) in the public project's `social_likes`; skills epic 2169, S6
// (ADR-0051 amendment 1, A1.5). A listing whose twin was deleted by an
// unpublish (`entity_id` null) has none.
func likeExpressions(publicSchema string, userID int64, args *argList) (likes, liked string) {
	likes = fmt.Sprintf(`CASE WHEN l.entity_id IS NOT NULL THEN
		    (SELECT count(*) FROM %s.social_likes sl
		     WHERE sl.entity_name = CASE l.kind WHEN 'skill' THEN 'skill' ELSE 'application' END
		       AND sl.entity_id = l.entity_id) ELSE 0 END`, publicSchema)
	liked = "false"
	if userID > 0 {
		liked = fmt.Sprintf(`(l.entity_id IS NOT NULL AND EXISTS (SELECT 1 FROM %s.social_likes sm
		    WHERE sm.entity_name = CASE l.kind WHEN 'skill' THEN 'skill' ELSE 'application' END
		      AND sm.entity_id = l.entity_id AND sm.user_id = %s))`, publicSchema, args.add(userID))
	}
	return likes, liked
}

func orderBy(p SearchParams, args *argList) string {
	switch p.Sort {
	case SortHires:
		return " ORDER BY hires DESC, l.created_at DESC, l.id DESC"
	case SortLikes:
		// `likes` is the output column of the page query's select list: the
		// count of the listing's kind (agents and skills together).
		return " ORDER BY likes DESC, hires DESC, l.created_at DESC, l.id DESC"
	case SortRating:
		return " ORDER BY l.rating_avg DESC NULLS LAST, l.rating_count DESC, l.id DESC"
	case SortNewest:
		return " ORDER BY l.created_at DESC, l.id DESC"
	case SortName:
		return " ORDER BY lower(l.name), l.id"
	}
	// Relevance: a match in the name first, then a prefix, then the
	// editorial flag and the hires.
	score := "0::int"
	if q := strings.ToLower(strings.TrimSpace(p.Query)); q != "" {
		name := args.add("%" + likeEscape(q) + "%")
		prefix := args.add(likeEscape(q) + "%")
		score = fmt.Sprintf(`(CASE WHEN lower(l.name) LIKE %s ESCAPE '\' THEN 2 ELSE 0 END + CASE WHEN lower(l.name) LIKE %s ESCAPE '\' THEN 1 ELSE 0 END)`, name, prefix)
	}
	return " ORDER BY " + score + " DESC, l.featured DESC, hires DESC, l.created_at DESC, l.id DESC"
}

// Search answers a page of listings and the facet counts.
func Search(ctx context.Context, db DB, p SearchParams) (Page, error) {
	if err := p.Validate(); err != nil {
		return Page{}, err
	}
	page := Page{Rows: []ListingSummary{}}

	countArgs := &argList{}
	if err := db.QueryRow(ctx,
		"SELECT count(*) FROM elitea_marketplace.listings l"+p.where(countArgs, facetNone), countArgs.values...).
		Scan(&page.Total); err != nil {
		return Page{}, fmt.Errorf("marketplace: count: %w", err)
	}

	args := &argList{}
	likes, liked := "0", "false"
	if p.PublicSchema != "" {
		likes, liked = likeExpressions(p.PublicSchema, p.UserID, args)
	}
	// The where clause is built after the select list so the parameters stay
	// numbered in the order they are used.
	where := p.where(args, facetNone)
	order := orderBy(p, args)
	sql := fmt.Sprintf(`
SELECT l.id, l.kind, COALESCE(l.entity_id, 0), l.name, l.description, l.icon, l.category, l.tags,
       l.publisher_id, COALESCE(au.name, ''), l.risk_tier, %s,
       (SELECT count(DISTINCT i.project_id) FROM elitea_marketplace.installs i WHERE i.listing_id = l.id AND i.dismissed_at IS NULL) AS hires,
       l.rating_avg::float8, l.rating_count, (%s)::int AS likes, %s, l.featured, l.required_types,
       l.has_write_tools, l.state
FROM elitea_marketplace.listings l
LEFT JOIN auth_core__user au ON au.id = l.publisher_id%s%s LIMIT %s OFFSET %s`,
		currentVersionSQL, likes, liked, where, order, args.add(p.Limit), args.add(p.Offset))
	rows, err := db.Query(ctx, sql, args.values...)
	if err != nil {
		return Page{}, fmt.Errorf("marketplace: page: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var (
			row        ListingSummary
			id, entity int64
			publisher  *int64
			pubName    string
			current    *string
		)
		if err := rows.Scan(&id, &row.Kind, &entity, &row.Name, &row.Description, &row.IconURL, &row.Category, &row.Tags,
			&publisher, &pubName, &row.Tier, &current, &row.Hires, &row.RatingAvg, &row.RatingCount, &row.Likes,
			&row.IsLiked, &row.Featured, &row.RequiredTypes, &row.HasWriteTools, &row.State); err != nil {
			rows.Close()
			return Page{}, fmt.Errorf("marketplace: scan: %w", err)
		}
		row.ID = strconv.FormatInt(id, 10)
		row.EntityID = strconv.FormatInt(entity, 10)
		row.CurrentVer = parseVersionRef(current)
		if publisher != nil {
			name := pubName
			if name == "" {
				name = "Publisher " + strconv.FormatInt(*publisher, 10)
			}
			row.Publisher = &Publisher{ID: strconv.FormatInt(*publisher, 10), Name: name}
		}
		if row.Tags == nil {
			row.Tags = []string{}
		}
		if row.RequiredTypes == nil {
			row.RequiredTypes = []string{}
		}
		row.Hired = []Hired{}
		page.Rows = append(page.Rows, row)
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	if p.ProjectID > 0 && len(ids) > 0 {
		hired, err := hiredIn(ctx, db, p.ProjectID, ids)
		if err != nil {
			return Page{}, err
		}
		for i := range page.Rows {
			id, _ := strconv.ParseInt(page.Rows[i].ID, 10, 64)
			if list := hired[id]; list != nil {
				page.Rows[i].Hired = list
			}
		}
	}
	facets, err := facetCounts(ctx, db, p)
	if err != nil {
		return Page{}, err
	}
	page.Facets = facets
	return page, nil
}

func parseVersionRef(packed *string) *VersionRef {
	if packed == nil {
		return nil
	}
	parts := strings.SplitN(*packed, "|", 3)
	if len(parts) != 3 {
		return nil
	}
	at, err := time.Parse(time.RFC3339, parts[2])
	if err != nil {
		return nil
	}
	return &VersionRef{ID: parts[0], Name: parts[1], PublishedAt: at}
}

// hiredIn lists the hires of the listings in one project.
func hiredIn(ctx context.Context, db DB, projectID int64, listingIDs []int64) (map[int64][]Hired, error) {
	rows, err := db.Query(ctx, `
SELECT i.listing_id, i.entity_id, i.mode, i.version_id, v.name
FROM elitea_marketplace.installs i
JOIN elitea_marketplace.listing_versions v ON v.listing_id = i.listing_id AND v.version_id = i.version_id
WHERE i.project_id = $1 AND i.dismissed_at IS NULL AND i.listing_id = ANY($2::bigint[])
ORDER BY i.created_at, i.entity_id`, projectID, listingIDs)
	if err != nil {
		return nil, fmt.Errorf("marketplace: read the hires: %w", err)
	}
	defer rows.Close()
	out := map[int64][]Hired{}
	for rows.Next() {
		var listing, entity, version int64
		var hired Hired
		if err := rows.Scan(&listing, &entity, &hired.Mode, &version, &hired.VersionName); err != nil {
			return nil, err
		}
		hired.ApplicationID = strconv.FormatInt(entity, 10)
		hired.VersionID = strconv.FormatInt(version, 10)
		out[listing] = append(out[listing], hired)
	}
	return out, rows.Err()
}

func facetCounts(ctx context.Context, db DB, p SearchParams) (Facets, error) {
	facets := Facets{
		Kinds: []FacetCount{}, Categories: []FacetCount{}, Tags: []FacetCount{},
		Publishers: []FacetCount{}, Tiers: []FacetCount{}, RequiredTypes: []FacetCount{},
	}
	group := func(exclude, valueExpr, labelExpr, join string, limit int) ([]FacetCount, error) {
		args := &argList{}
		where := p.where(args, exclude)
		sql := fmt.Sprintf(`SELECT %s AS value, %s AS label, count(*) FROM elitea_marketplace.listings l %s%s
AND %s <> '' GROUP BY 1, 2 ORDER BY 3 DESC, 2 LIMIT %d`, valueExpr, labelExpr, join, where, valueExpr, limit)
		rows, err := db.Query(ctx, sql, args.values...)
		if err != nil {
			return nil, fmt.Errorf("marketplace: facet %s: %w", exclude, err)
		}
		defer rows.Close()
		out := []FacetCount{}
		for rows.Next() {
			var fc FacetCount
			if err := rows.Scan(&fc.Value, &fc.Label, &fc.Count); err != nil {
				return nil, err
			}
			out = append(out, fc)
		}
		return out, rows.Err()
	}
	var err error
	if facets.Kinds, err = group(facetKind, "l.kind", "CASE l.kind WHEN 'agent' THEN 'Agents' WHEN 'team' THEN 'Teams' ELSE 'Skills' END", "", 5); err != nil {
		return Facets{}, err
	}
	if facets.Categories, err = group(facetCategory, "l.category", "l.category", "", 50); err != nil {
		return Facets{}, err
	}
	if facets.Tiers, err = group(facetTier, "l.risk_tier", "l.risk_tier", "", 4); err != nil {
		return Facets{}, err
	}
	if facets.Tags, err = group(facetTag, "t.tag", "t.tag", "CROSS JOIN LATERAL unnest(l.tags) AS t(tag)", 50); err != nil {
		return Facets{}, err
	}
	if facets.RequiredTypes, err = group(facetRequires, "r.type", "r.type", "CROSS JOIN LATERAL unnest(l.required_types) AS r(type)", 50); err != nil {
		return Facets{}, err
	}
	if facets.Publishers, err = group(facetPublisher, "l.publisher_id::text",
		"COALESCE(NULLIF(au.name, ''), 'Publisher ' || l.publisher_id::text)",
		"LEFT JOIN auth_core__user au ON au.id = l.publisher_id", 50); err != nil {
		return Facets{}, err
	}
	if p.ProjectID > 0 {
		args := &argList{}
		hiredFilter := p
		hiredFilter.HiredHere = false
		where := hiredFilter.where(args, facetNone)
		if err := db.QueryRow(ctx, `SELECT count(*) FROM elitea_marketplace.listings l`+where+
			` AND EXISTS (SELECT 1 FROM elitea_marketplace.installs hi WHERE hi.listing_id = l.id AND hi.dismissed_at IS NULL AND hi.project_id = `+args.add(p.ProjectID)+`)`,
			args.values...).Scan(&facets.HiredHere); err != nil {
			return Facets{}, fmt.Errorf("marketplace: facet hired here: %w", err)
		}
	}
	return facets, nil
}

// Detail is the listing page read.
type Detail struct {
	Listing              ListingSummary `json:"listing"`
	WelcomeMessage       string         `json:"welcome_message"`
	ConversationStarters []string       `json:"conversation_starters"`
	SoulSummary          string         `json:"soul_summary"`
	Versions             []VersionJSON  `json:"versions"`
	CanManage            bool           `json:"can_manage"`
	CanModerate          bool           `json:"can_moderate"`
	// Team is the members of a team listing (kind `team`); nil for an agent or
	// a skill.
	Team *TeamDetailJSON `json:"team,omitempty"`
}

// VersionJSON is the wire form of a version.
type VersionJSON struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	State        string       `json:"state"`
	StateReason  string       `json:"state_reason"`
	ReleaseNotes string       `json:"release_notes"`
	Digest       string       `json:"digest"`
	PublishedAt  time.Time    `json:"published_at"`
	Withdrawn    bool         `json:"withdrawn"`
	Requirements Requirements `json:"requirements"`
}

// GetParams says who reads a listing.
type GetParams struct {
	ListingID    int64
	ProjectID    int64
	UserID       int64
	PublicSchema string
	// Moderator is a platform administrator.
	Moderator bool
}

// Get reads one listing with its versions. A delisted listing reads only for a
// project that has it hired, for its publisher and for a moderator. It answers
// ErrNotFound for a listing the caller may not read, so a delisted listing is
// not told apart from a missing one.
func Get(ctx context.Context, db DB, p GetParams) (Detail, error) {
	var kind, state string
	var publisher *int64
	if err := db.QueryRow(ctx, `SELECT kind, state, publisher_id FROM elitea_marketplace.listings WHERE id = $1`, p.ListingID).
		Scan(&kind, &state, &publisher); errors.Is(err, pgx.ErrNoRows) {
		return Detail{}, ErrNotFound
	} else if err != nil {
		return Detail{}, fmt.Errorf("marketplace: read the listing: %w", err)
	}
	isPublisher := publisher != nil && *publisher == p.UserID && p.UserID > 0
	hired, err := hiredIn(ctx, db, p.ProjectID, []int64{p.ListingID})
	if err != nil {
		return Detail{}, err
	}
	if state == ListingDelisted && !p.Moderator && !isPublisher && len(hired[p.ListingID]) == 0 {
		return Detail{}, ErrNotFound
	}

	args := &argList{}
	likes, liked := "0", "false"
	if p.PublicSchema != "" {
		likes, liked = likeExpressions(p.PublicSchema, p.UserID, args)
	}
	var (
		row        ListingSummary
		id, entity int64
		pub        *int64
		pubName    string
		current    *string
	)
	err = db.QueryRow(ctx, fmt.Sprintf(`
SELECT l.id, l.kind, COALESCE(l.entity_id, 0), l.name, l.description, l.icon, l.category, l.tags,
       l.publisher_id, COALESCE(au.name, ''), l.risk_tier, %s,
       (SELECT count(DISTINCT i.project_id) FROM elitea_marketplace.installs i WHERE i.listing_id = l.id AND i.dismissed_at IS NULL),
       l.rating_avg::float8, l.rating_count, (%s)::int, %s, l.featured, l.required_types,
       l.has_write_tools, l.state
FROM elitea_marketplace.listings l LEFT JOIN auth_core__user au ON au.id = l.publisher_id
WHERE l.id = %s`, currentVersionSQL, likes, liked, args.add(p.ListingID)), args.values...).
		Scan(&id, &row.Kind, &entity, &row.Name, &row.Description, &row.IconURL, &row.Category, &row.Tags,
			&pub, &pubName, &row.Tier, &current, &row.Hires, &row.RatingAvg, &row.RatingCount, &row.Likes,
			&row.IsLiked, &row.Featured, &row.RequiredTypes, &row.HasWriteTools, &row.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return Detail{}, ErrNotFound
	}
	if err != nil {
		return Detail{}, fmt.Errorf("marketplace: read the listing card: %w", err)
	}
	row.ID = strconv.FormatInt(id, 10)
	row.EntityID = strconv.FormatInt(entity, 10)
	row.CurrentVer = parseVersionRef(current)
	if pub != nil {
		name := pubName
		if name == "" {
			name = "Publisher " + strconv.FormatInt(*pub, 10)
		}
		row.Publisher = &Publisher{ID: strconv.FormatInt(*pub, 10), Name: name}
	}
	if row.Tags == nil {
		row.Tags = []string{}
	}
	if row.RequiredTypes == nil {
		row.RequiredTypes = []string{}
	}
	row.Hired = hired[p.ListingID]
	if row.Hired == nil {
		row.Hired = []Hired{}
	}

	detail := Detail{Listing: row, ConversationStarters: []string{}, Versions: []VersionJSON{},
		CanManage: isPublisher, CanModerate: p.Moderator}
	versions, err := listVersions(ctx, db, p.ListingID, p.Moderator || isPublisher)
	if err != nil {
		return Detail{}, err
	}
	detail.Versions = versions
	if kind == KindAgent {
		if err := fillAgentDetail(ctx, db, p.ListingID, &detail); err != nil {
			return Detail{}, err
		}
	}
	if kind == KindTeam {
		if err := fillTeamDetail(ctx, db, p.ListingID, p.ProjectID, &detail); err != nil {
			return Detail{}, err
		}
	}
	return detail, nil
}

func fillAgentDetail(ctx context.Context, db DB, listingID int64, detail *Detail) error {
	var definition []byte
	err := db.QueryRow(ctx, `
SELECT definition FROM elitea_marketplace.listing_versions
WHERE listing_id = $1 AND withdrawn_at IS NULL AND state <> 'yanked'
ORDER BY published_at DESC, version_id DESC LIMIT 1`, listingID).Scan(&definition)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("marketplace: read the definition: %w", err)
	}
	node, err := ParseDefinition(definition)
	if err != nil {
		return err
	}
	detail.WelcomeMessage = node.Version.WelcomeMessage
	var starters []any
	if json.Unmarshal(node.Version.ConversationStarters, &starters) == nil {
		for _, starter := range starters {
			switch value := starter.(type) {
			case string:
				detail.ConversationStarters = append(detail.ConversationStarters, value)
			case map[string]any:
				if text, ok := value["text"].(string); ok {
					detail.ConversationStarters = append(detail.ConversationStarters, text)
				}
			}
		}
	}
	detail.SoulSummary = soulSummary(node.Version.Soul)
	return nil
}

func listVersions(ctx context.Context, db DB, listingID int64, includeWithdrawn bool) ([]VersionJSON, error) {
	rows, err := db.Query(ctx, `
SELECT version_id, name, state, state_reason, release_notes, digest, published_at, withdrawn_at IS NOT NULL, requirements
FROM elitea_marketplace.listing_versions
WHERE listing_id = $1 AND ($2 OR withdrawn_at IS NULL)
ORDER BY published_at DESC, version_id DESC`, listingID, includeWithdrawn)
	if err != nil {
		return nil, fmt.Errorf("marketplace: read the versions: %w", err)
	}
	defer rows.Close()
	out := []VersionJSON{}
	currentSeen := false
	for rows.Next() {
		var (
			v       VersionJSON
			id      int64
			digest  []byte
			reqsRaw []byte
		)
		if err := rows.Scan(&id, &v.Name, &v.State, &v.StateReason, &v.ReleaseNotes, &digest, &v.PublishedAt, &v.Withdrawn, &reqsRaw); err != nil {
			return nil, err
		}
		v.ID = strconv.FormatInt(id, 10)
		v.Digest = hex.EncodeToString(digest)
		_ = json.Unmarshal(reqsRaw, &v.Requirements) // a malformed column reads as no requirements
		if v.Requirements.Items == nil {
			v.Requirements.Items = []Requirement{}
		}
		// The newest hireable version is current, unless it is deprecated.
		if !currentSeen && !v.Withdrawn && v.State != VersionYanked {
			currentSeen = true
			if v.State == VersionAvailable {
				v.State = VersionCurrent
			}
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
