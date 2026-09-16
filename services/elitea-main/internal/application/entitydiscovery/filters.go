// Package entitydiscovery owns actor-scoped tags and search option reads.
package entitydiscovery

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type Tag struct {
	ID               int    `json:"id"`
	Name             string `json:"name"`
	Data             any    `json:"data"`
	ApplicationCount *int   `json:"application_count,omitempty"`
	SkillCount       *int   `json:"skill_count,omitempty"`
}
type Page[T any] struct {
	Total int `json:"total"`
	Rows  []T `json:"rows"`
}
type Filters struct {
	tagSort, tagOrder    string
	Coverage             string
	Query, Search        string
	AuthorID             int64
	Statuses             []string
	Tags                 []int64
	Limit, Offset        int
	MyLiked              bool
	TrendStart, TrendEnd *time.Time
}

func Parse(values url.Values) (Filters, error) {
	f := Filters{Coverage: values.Get("entity_coverage"), Query: values.Get("query"), Search: values.Get("search"), Limit: 1000}
	if f.Coverage == "" {
		f.Coverage = "all"
	}
	switch f.Coverage {
	case "all", "application", "pipeline", "skill":
	default:
		return f, apierr.BadRequest("invalid entity_coverage")
	}
	if len(f.Query) > 1024 || len(f.Search) > 1024 {
		return f, apierr.BadRequest("search is too long")
	}
	var err error
	f.Limit, err = boundedInt(values.Get("limit"), 1000, 0, 1000)
	if err != nil {
		return f, err
	}
	if f.Limit == 0 {
		f.Limit = 1000
	}
	f.Offset, err = boundedInt(values.Get("offset"), 0, 0, 100000)
	if err != nil {
		return f, err
	}
	if raw := values.Get("author_id"); raw != "" {
		f.AuthorID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || f.AuthorID <= 0 {
			return f, apierr.BadRequest("invalid author_id")
		}
	}
	f.Statuses = values["statuses[]"]
	if len(f.Statuses) == 0 && values.Get("statuses") != "" {
		f.Statuses = strings.Split(values.Get("statuses"), ",")
	}
	if len(f.Statuses) > 32 {
		return f, apierr.BadRequest("too many statuses")
	}
	for _, status := range f.Statuses {
		if len(status) > 64 {
			return f, apierr.BadRequest("invalid status")
		}
	}
	if len(values["tags[]"]) > 100 {
		return f, apierr.BadRequest("too many tags")
	}
	seenTags := map[int64]bool{}
	for _, raw := range values["tags[]"] {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return f, apierr.BadRequest("invalid tag id")
		}
		if !seenTags[id] {
			f.Tags = append(f.Tags, id)
			seenTags[id] = true
		}
	}
	if raw := values.Get("my_liked"); raw != "" {
		f.MyLiked, err = strconv.ParseBool(raw)
		if err != nil {
			return f, apierr.BadRequest("invalid my_liked")
		}
	}
	for _, entry := range []struct {
		name   string
		target **time.Time
	}{{"trend_start_period", &f.TrendStart}, {"trend_end_period", &f.TrendEnd}} {
		if raw := values.Get(entry.name); raw != "" {
			value, err := time.Parse("2006-01-02T15:04:05", raw)
			if err != nil {
				return f, apierr.BadRequest("invalid trend period")
			}
			*entry.target = &value
		}
	}
	if f.TrendEnd != nil && f.TrendStart == nil {
		return f, apierr.BadRequest("trend start is required")
	}
	if f.TrendStart != nil && f.TrendEnd != nil && f.TrendStart.After(*f.TrendEnd) {
		return f, apierr.BadRequest("invalid trend period")
	}
	return f, nil
}
func boundedInt(raw string, fallback, min, max int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < min || n > max {
		return 0, apierr.BadRequest("pagination exceeds allowed bounds")
	}
	return n, nil
}

func (f Filters) validate() error {
	switch f.Coverage {
	case "all", "application", "pipeline", "skill":
	default:
		return apierr.BadRequest("invalid entity_coverage")
	}
	if f.Limit < 1 || f.Limit > 1000 || f.Offset < 0 || f.Offset > 100000 || len(f.Query) > 1024 || len(f.Search) > 1024 || len(f.Tags) > 100 || len(f.Statuses) > 32 {
		return apierr.BadRequest("invalid discovery filters")
	}
	return nil
}
