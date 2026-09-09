package applications

import (
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// ValidateList bounds work before a repository query and sets stable defaults.
func (req *ListRequest) ValidateList() error {
	if req.Page < 1 {
		req.Page = 1
	}
	if req.PageSize == 0 {
		req.PageSize = 20
	}
	if req.PageSize < 1 || req.PageSize > 1000 {
		return apierr.BadRequest("page size must be between 1 and 1000")
	}
	if req.Page > 100001 || (req.Page-1)*req.PageSize > 1000000 {
		return apierr.BadRequest("offset must be between 0 and 100000")
	}
	if req.Offset != nil {
		if *req.Offset < 0 || *req.Offset > 100000 {
			return apierr.BadRequest("offset must be between 0 and 100000")
		}
		req.Page = *req.Offset/req.PageSize + 1
	}
	if len(req.IDs) > 100 {
		return apierr.BadRequest("ids accepts at most 100 entries")
	}
	for _, id := range req.IDs {
		if id <= 0 {
			return apierr.BadRequest("ids must contain positive integers")
		}
	}
	if req.AuthorID < 0 || req.AuthorID > 2147483647 {
		return apierr.BadRequest("invalid author_id")
	}
	if len(req.Search) > 1024 || len(req.Tags) > 12800 || len(strings.Split(req.Tags, ",")) > 100 {
		return apierr.BadRequest("search or tags exceed supported bounds")
	}
	if len(req.Statuses) > 32 {
		return apierr.BadRequest("too many statuses")
	}
	for i, status := range req.Statuses {
		status = strings.TrimSpace(status)
		if status == "" || len(status) > 64 {
			return apierr.BadRequest("invalid status")
		}
		req.Statuses[i] = status
	}
	req.AgentsType = strings.ToLower(strings.TrimSpace(req.AgentsType))
	switch req.AgentsType {
	case "", "all", "classic", "pipeline":
	default:
		return apierr.BadRequest("agents_type must be all, classic, or pipeline")
	}
	if req.FolderID != "" {
		return apierr.BadRequest("folder_id is not supported; use ids for folder contents")
	}
	if req.SortBy == "" {
		req.SortBy = "created_at"
	}
	switch req.SortBy {
	case "created_at", "updated_at", "name", "id", "author", "authors", "likes":
	default:
		return apierr.BadRequest("unsupported sort_by")
	}
	if req.SortOrder == "" {
		req.SortOrder = "desc"
	}
	req.SortOrder = strings.ToLower(req.SortOrder)
	if req.SortOrder != "asc" && req.SortOrder != "desc" {
		return apierr.BadRequest("sort_order must be asc or desc")
	}
	if req.TrendEnd != nil && req.TrendStart == nil {
		return apierr.BadRequest("trend start is required")
	}
	if req.TrendStart != nil && req.TrendEnd != nil && req.TrendStart.After(*req.TrendEnd) {
		return apierr.BadRequest("trend start must precede trend end")
	}
	return nil
}
