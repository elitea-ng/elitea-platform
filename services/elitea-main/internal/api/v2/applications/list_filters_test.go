package applications

import (
	"context"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
	"github.com/go-chi/chi/v5"
)

type applicationListRecorder struct {
	domain.Repository
	request domain.ListRequest
	calls   int
}

func (r *applicationListRecorder) List(_ context.Context, request domain.ListRequest) (domain.ListResponse, error) {
	r.request = request
	r.calls++
	return domain.ListResponse{Rows: []domain.Application{}, Total: 0}, nil
}

func TestApplicationListHTTPForwardsValidatedFiltersAndExactOffset(t *testing.T) {
	repo := &applicationListRecorder{}
	router := chi.NewRouter()
	router.Get("/apps/{projectID}", NewHandler(repo).List)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", "/apps/7?ids=3,4&tags=red,blue&statuses=draft,published&author_id=9&my_liked=true&without_tags=false&agents_type=all&limit=3&offset=4&sort_by=name&sort_order=asc&trend_start_period=2026-01-01T00:00:00&trend_end_period=2026-02-01T00:00:00", nil))
	req := repo.request
	if response.Code != 200 || repo.calls != 1 || req.ProjectID != "7" || req.Offset == nil || *req.Offset != 4 || req.Page != 2 || req.PageSize != 3 || !req.MyLiked || req.AuthorID != 9 || req.SortBy != "name" || req.SortOrder != "asc" || req.TrendStart == nil || req.TrendEnd == nil || !reflect.DeepEqual(req.IDs, []int32{3, 4}) || !reflect.DeepEqual(req.Statuses, []string{"draft", "published"}) || req.Tags != "red,blue" {
		t.Fatalf("status=%d request=%+v", response.Code, req)
	}
}

func TestApplicationListRejectsUnboundedOrInvalidFilters(t *testing.T) {
	for _, query := range []string{"ids=0", "ids=3.5", "ids=" + strings.Repeat("1,", 100) + "2", "limit=101", "offset=100001", "offset=-1", "author_id=2147483648", "statuses=,draft", "my_liked=maybe", "agents_type=wrong", "sort_by=name;DROP", "sort_order=DESC;DROP", "trend_end_period=2026-01-01T00:00:00", "trend_start_period=bad", "trend_start_period=2026-02-01T00:00:00&trend_end_period=2026-01-01T00:00:00", "query=" + strings.Repeat("x", 1025), "folder_id=4"} {
		t.Run(query[:min(len(query), 40)], func(t *testing.T) {
			repo := &applicationListRecorder{}
			router := chi.NewRouter()
			router.Get("/apps/{projectID}", NewHandler(repo).List)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("GET", "/apps/7?"+query, nil))
			if response.Code != 400 || repo.calls != 0 {
				t.Fatalf("status=%d calls=%d", response.Code, repo.calls)
			}
		})
	}
}

func TestApplicationListAcceptsArrayTagAndStatusQueryParameters(t *testing.T) {
	req, err := parseApplicationList(url.Values{"tags[]": {"red", "blue"}, "statuses[]": {"published", "draft"}})
	if err != nil || req.Tags != "red,blue" || len(req.Statuses) != 2 {
		t.Fatalf("request=%+v err=%v", req, err)
	}
}
