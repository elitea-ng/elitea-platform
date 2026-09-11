package entitydiscovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/foldervisibility"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/publicproject"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

type predicates struct{ args []any }

func (p *predicates) bind(v any) string {
	p.args = append(p.args, v)
	return fmt.Sprintf("$%d", len(p.args))
}
func entityTables(kind string) (table, versions, association, key, folder string) {
	if kind == "skill" {
		return "skills", "skill_versions", "skill_version_tag_association", "skill_id", "skill"
	}
	folder = "agent"
	if kind == "pipeline" {
		folder = "pipeline"
	}
	return "applications", "application_versions", "application_version_tag_association", "application_id", folder
}
func eligibility(s, kind string, f Filters, a foldervisibility.Access, p *predicates) string {
	_, versions, association, key, folder := entityTables(kind)
	where := []string{"TRUE"}
	if kind != "skill" {
		operator := "NOT EXISTS"
		if kind == "pipeline" {
			operator = "EXISTS"
		}
		where = append(where, fmt.Sprintf("%s (SELECT 1 FROM %s.application_versions pv WHERE pv.application_id=e.id AND pv.agent_type='pipeline')", operator, s))
	}
	if f.Query != "" {
		value := p.bind("%" + f.Query + "%")
		where = append(where, "(e.name ILIKE "+value+" OR e.description ILIKE "+value+")")
	}
	if f.AuthorID > 0 {
		where = append(where, fmt.Sprintf("EXISTS (SELECT 1 FROM %s.%s fv WHERE fv.%s=e.id AND fv.author_id=%s)", s, versions, key, p.bind(f.AuthorID)))
	}
	if len(f.Statuses) > 0 && kind != "skill" {
		where = append(where, fmt.Sprintf("EXISTS (SELECT 1 FROM %s.%s fv WHERE fv.%s=e.id AND fv.status=ANY(%s::text[]))", s, versions, key, p.bind(f.Statuses)))
	}
	if len(f.Tags) > 0 {
		where = append(where, fmt.Sprintf("(SELECT COUNT(DISTINCT fa.tag_id) FROM %s.%s fv JOIN %s.%s fa ON fa.version_id=fv.id WHERE fv.%s=e.id AND fa.tag_id=ANY(%s::bigint[]))=%s", s, versions, s, association, key, p.bind(f.Tags), p.bind(len(f.Tags))))
	}
	if a.FolderRestrictions {
		where = append(where, foldervisibility.ExclusionSQL(s, "e.id", p.bind([]string{folder}), p.bind(a.ActorID)))
	}
	if kind != "skill" && f.MyLiked {
		where = append(where, fmt.Sprintf("EXISTS (SELECT 1 FROM %s.social_likes l WHERE l.entity_name='application' AND l.entity_id=e.id AND l.user_id=%s)", s, p.bind(a.ActorID)))
	}
	if kind != "skill" && f.TrendStart != nil {
		end := time.Now().UTC()
		if f.TrendEnd != nil {
			end = *f.TrendEnd
		}
		where = append(where, fmt.Sprintf("EXISTS (SELECT 1 FROM %s.social_likes l WHERE l.entity_name='application' AND l.entity_id=e.id AND l.created_at BETWEEN %s AND %s)", s, p.bind(*f.TrendStart), p.bind(end)))
	}
	return strings.Join(where, " AND ")
}
func (svc *Service) access(ctx context.Context, project string) (string, foldervisibility.Access, error) {
	s, err := tenantschema.Quote(project)
	if err != nil {
		return "", foldervisibility.Access{}, err
	}
	a, err := foldervisibility.Resolve(ctx, svc.pool, s)
	return s, a, err
}

// Tags counts distinct entities. All coverage merges the three ordered pages.
func (svc *Service) Tags(ctx context.Context, project string, f Filters) (Page[Tag], error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Page[Tag]{}, err
	}
	if err := f.validate(); err != nil {
		return Page[Tag]{}, err
	}
	s, a, err := svc.access(ctx, project)
	if err != nil {
		return Page[Tag]{}, err
	}
	tx, err := svc.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Page[Tag]{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	kinds := []string{f.Coverage}
	if f.Coverage == "all" {
		kinds = []string{"application", "pipeline", "skill"}
	}
	result := Page[Tag]{Rows: []Tag{}}
	seen := map[int]bool{}
	for _, kind := range kinds {
		page, err := tagPage(ctx, tx, s, kind, f, a)
		if err != nil {
			return Page[Tag]{}, fmt.Errorf("list discovery tags: %w", err)
		}
		if f.Coverage != "all" {
			result = page
			break
		}
		for _, tag := range page.Rows {
			if !seen[tag.ID] {
				seen[tag.ID] = true
				result.Rows = append(result.Rows, tag)
			}
		}
	}
	if f.Coverage == "all" {
		result.Total = len(result.Rows)
	}
	if err := tx.Commit(ctx); err != nil {
		return Page[Tag]{}, err
	}
	return result, nil
}
func tagPage(ctx context.Context, tx pgx.Tx, s, kind string, f Filters, a foldervisibility.Access) (Page[Tag], error) {
	table, versions, association, key, _ := entityTables(kind)
	p := &predicates{}
	where := eligibility(s, kind, f, a, p)
	if f.Search != "" {
		where += " AND t.name ILIKE " + p.bind("%"+f.Search+"%")
	}
	countName := "application_count"
	if kind == "skill" {
		countName = "skill_count"
	}
	orderBy := countName + " DESC,id DESC"
	if f.tagSort != "" {
		if f.tagSort != "id" && f.tagSort != "name" {
			return Page[Tag]{}, apierr.BadRequest("invalid tag sort")
		}
		if f.tagOrder != "asc" && f.tagOrder != "desc" {
			return Page[Tag]{}, apierr.BadRequest("invalid tag order")
		}
		orderBy = f.tagSort + " " + f.tagOrder + ",id " + f.tagOrder
	}
	query := fmt.Sprintf(`WITH matching AS (
 SELECT t.id,t.name,t.data::jsonb AS data,COUNT(DISTINCT e.id)::integer AS %s
 FROM %s.tags t JOIN %s.%s a ON a.tag_id=t.id JOIN %s.%s v ON v.id=a.version_id
 JOIN %s.%s e ON e.id=v.%s WHERE %s GROUP BY t.id,t.name,t.data::jsonb),
 page AS (SELECT * FROM matching ORDER BY %s LIMIT %s OFFSET %s)
 SELECT (SELECT COUNT(*) FROM matching),COALESCE((SELECT jsonb_agg(to_jsonb(page)) FROM page),'[]'::jsonb)`, countName, s, s, association, s, versions, s, table, key, where, orderBy, p.bind(f.Limit), p.bind(f.Offset))
	result := Page[Tag]{Rows: []Tag{}}
	var raw []byte
	if err := tx.QueryRow(ctx, query, p.args...).Scan(&result.Total, &raw); err != nil {
		return result, err
	}
	if err := json.Unmarshal(raw, &result.Rows); err != nil {
		return result, err
	}
	return result, nil
}

// SearchOptions returns the current seven-section envelope with bounded pages.
func (svc *Service) SearchOptions(ctx context.Context, project string, values url.Values) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := Parse(values)
	if err != nil {
		return nil, err
	}
	entities := values["entities[]"]
	if len(entities) > 7 {
		return nil, apierr.BadRequest("too many entities")
	}
	requested := map[string]bool{}
	for _, kind := range entities {
		switch kind {
		case "application", "pipeline", "toolkit", "credential", "skill", "tag", "collection":
			requested[kind] = true
		default:
			return nil, apierr.BadRequest("invalid entity")
		}
	}
	s, a, err := svc.access(ctx, project)
	if err != nil {
		return nil, err
	}
	tx, err := svc.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result := map[string]any{}
	for _, kind := range []string{"application", "pipeline", "toolkit", "credential", "skill", "tag", "collection"} {
		result[kind] = Page[map[string]any]{Rows: []map[string]any{}}
	}
	for _, kind := range []string{"application", "pipeline", "toolkit", "credential", "skill"} {
		if !requested[kind] {
			continue
		}
		pageFilter := f
		if (kind == "toolkit" || kind == "credential") && values.Get("limit") == "" {
			pageFilter.Limit = 10
		}
		for _, name := range []string{"limit", "offset"} {
			if raw := values.Get(kind + "_" + name); raw != "" {
				v := cloneValues(values)
				v.Set(name, raw)
				parsed, e := Parse(v)
				if e != nil {
					return nil, e
				}
				if name == "limit" {
					pageFilter.Limit = parsed.Limit
				} else {
					pageFilter.Offset = parsed.Offset
				}
			}
		}
		page, err := searchPage(ctx, tx, s, kind, pageFilter, a, values, false)
		if err != nil {
			return nil, fmt.Errorf("list search options: %w", err)
		}
		if kind == "credential" {
			if raw := values.Get("include_shared"); raw != "" {
				enabled, e := strconv.ParseBool(raw)
				if e != nil {
					return nil, apierr.BadRequest("invalid include_shared")
				}
				if enabled && project != strconv.Itoa(publicproject.ID()) {
					publicSchema, e := tenantschema.Quote(strconv.Itoa(publicproject.ID()))
					if e != nil {
						return nil, e
					}
					sharedFilter := f
					sharedFilter.Limit, e = boundedInt(values.Get("shared_limit"), 10, 1, 1000)
					if e != nil {
						return nil, e
					}
					sharedFilter.Offset, e = boundedInt(values.Get("shared_offset"), 0, 0, 100000)
					if e != nil {
						return nil, e
					}
					shared, e := searchPage(ctx, tx, publicSchema, kind, sharedFilter, a, values, true)
					if e != nil {
						return nil, e
					}
					page.Rows = append(page.Rows, shared.Rows...)
				}
			}
			// Current credential search options carry rows without a total field.
			result[kind] = map[string]any{"rows": page.Rows}
		} else {
			result[kind] = page
		}
	}
	if requested["tag"] {
		tags := []Tag{}
		seen := map[int]bool{}
		for _, kind := range []string{"application", "pipeline"} {
			if !requested[kind] {
				continue
			}
			tf := f
			tf.Query = ""
			tf.Search = f.Query
			tf.tagSort = values.Get("tag_sort")
			if tf.tagSort == "" {
				tf.tagSort = values.Get("sort")
			}
			if tf.tagSort == "" {
				tf.tagSort = "id"
			}
			tf.tagOrder = values.Get("tag_order")
			if tf.tagOrder == "" {
				tf.tagOrder = values.Get("order")
			}
			if tf.tagOrder == "" {
				tf.tagOrder = "desc"
			}
			for _, name := range []string{"limit", "offset"} {
				if raw := values.Get("tag_" + name); raw != "" {
					v := cloneValues(values)
					v.Set(name, raw)
					parsed, e := Parse(v)
					if e != nil {
						return nil, e
					}
					if name == "limit" {
						tf.Limit = parsed.Limit
					} else {
						tf.Offset = parsed.Offset
					}
				}
			}
			page, e := tagPage(ctx, tx, s, kind, tf, a)
			if e != nil {
				return nil, e
			}
			for _, tag := range page.Rows {
				if !seen[tag.ID] {
					seen[tag.ID] = true
					tag.ApplicationCount = nil
					tags = append(tags, tag)
				}
			}
		}
		result["tag"] = Page[Tag]{Total: len(tags), Rows: tags}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}
func searchPage(ctx context.Context, tx pgx.Tx, s, kind string, f Filters, a foldervisibility.Access, values url.Values, shared bool) (Page[map[string]any], error) {
	p := &predicates{}
	table, _, _, _, _ := entityTables(kind)
	name := "e.name"
	projection := "e.id,e.name"
	where := "TRUE"
	sort := "e.name"
	switch kind {
	case "application", "pipeline", "skill":
		if kind == "skill" {
			f.AuthorID = 0
			f.Statuses = nil
			f.Tags = nil
		}
		query := f.Query
		if kind != "skill" {
			f.Query = ""
		}
		where = eligibility(s, kind, f, a, p)
		if kind != "skill" && query != "" {
			where += " AND e.name ILIKE " + p.bind("%"+query+"%")
		}
	case "toolkit":
		table = "elitea_tools"
		name = "COALESCE(NULLIF(e.name,''),NULLIF(e.settings->>'elitea_title',''),NULLIF(e.settings->>'configuration_title',''),e.settings->>'toolkit_name')"
		projection = "e.id," + name + " AS name,e.type"
		if typ := values.Get("toolkit_type"); typ != "" {
			if len(typ) > 128 {
				return Page[map[string]any]{}, apierr.BadRequest("invalid toolkit_type")
			}
			where += " AND e.type=" + p.bind(typ)
		}
		if a.FolderRestrictions {
			where += " AND " + foldervisibility.ExclusionSQL(s, "e.id", p.bind([]string{"toolkit"}), p.bind(a.ActorID))
		}
	case "credential":
		table = "configuration"
		name = "e.label"
		projection = "e.id,e.label AS name"
		section := values.Get("section")
		if section == "" {
			section = "credentials"
		}
		if len(section) > 128 {
			return Page[map[string]any]{}, apierr.BadRequest("invalid section")
		}
		where += " AND e.section=" + p.bind(section)
		if typ := values.Get("type_filter"); typ != "" {
			if len(typ) > 128 {
				return Page[map[string]any]{}, apierr.BadRequest("invalid type_filter")
			}
			where += " AND e.type=" + p.bind(typ)
		}
		if shared {
			where += " AND e.shared=true"
		}
	}
	if f.Query != "" && (kind == "toolkit" || kind == "credential") {
		where += " AND " + name + " ILIKE " + p.bind("%"+f.Query+"%")
	}
	sort = values.Get(kind + "_sort")
	if sort == "" {
		sort = values.Get("sort")
	}
	if sort == "" {
		sort = "id"
	}
	switch sort {
	case "id", "name", "created_at":
	default:
		return Page[map[string]any]{}, apierr.BadRequest("invalid sort")
	}
	sortExpr := "e." + sort
	if sort == "name" {
		sortExpr = name
	}
	order := values.Get(kind + "_order")
	if order == "" {
		order = values.Get("order")
	}
	if order == "" {
		order = "desc"
	}
	if order != "asc" && order != "desc" {
		return Page[map[string]any]{}, apierr.BadRequest("invalid order")
	}
	query := fmt.Sprintf(`WITH matching AS (SELECT %s,%s AS sort_value FROM %s.%s e WHERE %s),
 page AS (SELECT * FROM matching ORDER BY sort_value %s,id %s LIMIT %s OFFSET %s)
 SELECT (SELECT COUNT(*) FROM matching),COALESCE((SELECT jsonb_agg(to_jsonb(page)-'sort_value') FROM page),'[]'::jsonb)`, projection, sortExpr, s, table, where, order, order, p.bind(f.Limit), p.bind(f.Offset))
	page := Page[map[string]any]{Rows: []map[string]any{}}
	var raw []byte
	if err := tx.QueryRow(ctx, query, p.args...).Scan(&page.Total, &raw); err != nil {
		return page, err
	}
	if err := json.Unmarshal(raw, &page.Rows); err != nil {
		return page, err
	}
	return page, nil
}

func cloneValues(values url.Values) url.Values {
	result := url.Values{}
	for key, entries := range values {
		result[key] = append([]string(nil), entries...)
	}
	return result
}
