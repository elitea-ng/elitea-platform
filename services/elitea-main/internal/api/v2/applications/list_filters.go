package applications

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

func parseApplicationList(values url.Values) (applications.ListRequest, error) {
	req := applications.ListRequest{Page: 1, PageSize: 20, Search: values.Get("query"), Tags: values.Get("tags"), AgentsType: values.Get("agents_type"), SortBy: values.Get("sort_by"), SortOrder: values.Get("sort_order"), FolderID: values.Get("folder_id")}
	if req.Search == "" {
		req.Search = values.Get("search")
	}
	if req.Tags == "" {
		req.Tags = strings.Join(values["tags[]"], ",")
	}
	if raw := values.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return req, apierr.BadRequest("invalid limit")
		}
		req.PageSize = n
	}
	if req.PageSize < 1 || req.PageSize > 100 {
		return req, apierr.BadRequest("limit must be between 1 and 100")
	}
	offset := 0
	if raw := values.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return req, apierr.BadRequest("invalid offset")
		}
		offset = n
	}
	req.Offset = &offset
	if raw := values.Get("ids"); raw != "" {
		if len(raw) > 1099 {
			return req, apierr.BadRequest("ids exceeds supported bounds")
		}
		parts := strings.Split(raw, ",")
		if len(parts) > 100 {
			return req, apierr.BadRequest("ids accepts at most 100 entries")
		}
		for _, part := range parts {
			id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 32)
			if err != nil || id <= 0 {
				return req, apierr.BadRequest("ids must contain positive integers")
			}
			req.IDs = append(req.IDs, int32(id))
		}
	}
	if raw := values.Get("author_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || id <= 0 {
			return req, apierr.BadRequest("author_id must be a positive integer")
		}
		req.AuthorID = id
	}
	if len(values.Get("statuses")) > 2079 {
		return req, apierr.BadRequest("statuses exceed supported bounds")
	}
	req.Statuses = values["statuses[]"]
	if len(req.Statuses) == 0 && values.Get("statuses") != "" {
		req.Statuses = strings.Split(values.Get("statuses"), ",")
	}
	for _, entry := range []struct {
		name   string
		target *bool
	}{{"my_liked", &req.MyLiked}, {"without_tags", &req.WithoutTags}} {
		if raw := values.Get(entry.name); raw != "" {
			v, err := strconv.ParseBool(raw)
			if err != nil {
				return req, apierr.BadRequest("invalid " + entry.name)
			}
			*entry.target = v
		}
	}
	for _, entry := range []struct {
		name   string
		target **time.Time
	}{{"trend_start_period", &req.TrendStart}, {"trend_end_period", &req.TrendEnd}} {
		if raw := values.Get(entry.name); raw != "" {
			v, err := time.Parse("2006-01-02T15:04:05", raw)
			if err != nil {
				v, err = time.Parse(time.RFC3339, raw)
			}
			if err != nil {
				return req, apierr.BadRequest("invalid trend period")
			}
			*entry.target = &v
		}
	}
	return req, req.ValidateList()
}
