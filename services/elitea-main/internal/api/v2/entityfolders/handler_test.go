package entityfolders_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/entityfolders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/folders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

type fakeRepo struct {
	list       []entityfolders.Folder
	rebalanced []entityfolders.Folder
	err        error

	kind       entityfolders.Kind
	createName string
	updName    *string
	updPos     *int
	rebalances int
	movedID    int
	movedTo    *string
	deleted    string
}

func (f *fakeRepo) List(_ context.Context, _ string, _ entityfolders.Kind) ([]entityfolders.Folder, error) {
	return f.list, f.err
}

func (f *fakeRepo) Create(_ context.Context, _ string, k entityfolders.Kind, name string, _ *int) (entityfolders.Folder, error) {
	f.kind, f.createName = k, name
	return entityfolders.Folder{ID: "9", Kind: k, Name: name, ItemIDs: []int{}}, f.err
}

func (f *fakeRepo) Update(_ context.Context, _ string, k entityfolders.Kind, id string, name *string, pos *int) (entityfolders.Folder, error) {
	f.kind, f.updName, f.updPos = k, name, pos
	return entityfolders.Folder{ID: id, Kind: k}, f.err
}

func (f *fakeRepo) Delete(_ context.Context, _ string, _ entityfolders.Kind, id string) error {
	f.deleted = id
	return f.err
}

func (f *fakeRepo) Rebalance(_ context.Context, _ string, _ entityfolders.Kind) ([]entityfolders.Folder, error) {
	f.rebalances++
	return f.rebalanced, f.err
}

func (f *fakeRepo) Move(_ context.Context, _ string, _ entityfolders.Kind, id int, to *string) error {
	f.movedID, f.movedTo = id, to
	return f.err
}

func serve(repo *fakeRepo, method, path, body string) *httptest.ResponseRecorder {
	h := entityfolders.NewHandler(repo)
	r := chi.NewRouter()
	base := "/p/{projectID}/{kind}"
	r.Get(base, h.List)
	r.Post(base, h.Create)
	r.Put(base+"/items/{entityID}", h.Move)
	r.Put(base+"/{folderID}", h.Update)
	r.Delete(base+"/{folderID}", h.Delete)
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func pos(v int) *int { return &v }

func TestCreateTrimsNameAndCarriesKind(t *testing.T) {
	repo := &fakeRepo{}
	rec := serve(repo, http.MethodPost, "/p/7/skill", `{"name":"  Drafts  "}`)
	require.Equal(t, http.StatusCreated, rec.Code)
	require.Equal(t, "Drafts", repo.createName)
	require.Equal(t, entityfolders.KindSkill, repo.kind)
	require.Contains(t, rec.Body.String(), `"kind":"skill"`)
}

func TestCreateRefusesBlankAndOverlongNames(t *testing.T) {
	for _, body := range []string{`{"name":"   "}`, `{}`, `{"name":"` + string(bytes.Repeat([]byte("x"), 129)) + `"}`, `nope`} {
		rec := serve(&fakeRepo{}, http.MethodPost, "/p/7/skill", body)
		require.Equal(t, http.StatusBadRequest, rec.Code, body)
	}
}

func TestRenameLeavesPositionAlone(t *testing.T) {
	repo := &fakeRepo{}
	rec := serve(repo, http.MethodPut, "/p/7/skill/3", `{"name":"Renamed"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "Renamed", *repo.updName)
	require.Nil(t, repo.updPos, "a rename must not move the folder")
}

func TestReorderNeighboursBeatPosition(t *testing.T) {
	repo := &fakeRepo{list: []entityfolders.Folder{
		{ID: "1", Position: 3 * folders.PositionGap},
		{ID: "2", Position: 2 * folders.PositionGap},
		{ID: "3", Position: 1 * folders.PositionGap},
	}}
	// Dropped between 1 (above) and 2 (below); the client's own number is wrong.
	rec := serve(repo, http.MethodPut, "/p/7/skill/3", `{"position":5,"neighbor_above_id":"1","neighbor_below_id":2}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Nil(t, repo.updName)
	require.Equal(t, pos((3*folders.PositionGap+2*folders.PositionGap)/2), repo.updPos)
	require.Zero(t, repo.rebalances)
}

func TestReorderToTopSitsOneGapAboveTheFolderBelow(t *testing.T) {
	repo := &fakeRepo{list: []entityfolders.Folder{{ID: "1", Position: 2 * folders.PositionGap}, {ID: "3", Position: 1}}}
	rec := serve(repo, http.MethodPut, "/p/7/skill/3", `{"neighbor_above_id":null,"neighbor_below_id":"1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, pos(3*folders.PositionGap), repo.updPos)
}

func TestReorderRebalancesWhenNeighboursAreAdjacent(t *testing.T) {
	repo := &fakeRepo{
		list:       []entityfolders.Folder{{ID: "1", Position: 11}, {ID: "2", Position: 10}, {ID: "3", Position: 1}},
		rebalanced: []entityfolders.Folder{{ID: "1", Position: 2 * folders.PositionGap}, {ID: "2", Position: folders.PositionGap}},
	}
	rec := serve(repo, http.MethodPut, "/p/7/skill/3", `{"neighbor_above_id":"1","neighbor_below_id":"2"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, repo.rebalances)
	require.Equal(t, pos(3*folders.PositionGap/2), repo.updPos)
}

func TestMovePutsIntoAndOutOfAFolder(t *testing.T) {
	repo := &fakeRepo{}
	require.Equal(t, http.StatusNoContent, serve(repo, http.MethodPut, "/p/7/skill/items/42", `{"folder_id":"3"}`).Code)
	require.Equal(t, 42, repo.movedID)
	require.Equal(t, "3", *repo.movedTo)

	require.Equal(t, http.StatusNoContent, serve(repo, http.MethodPut, "/p/7/skill/items/42", `{"folder_id":null}`).Code)
	require.Nil(t, repo.movedTo, "null takes the entity back to the root")
	require.Equal(t, http.StatusNoContent, serve(repo, http.MethodPut, "/p/7/skill/items/42", `{}`).Code)
	require.Nil(t, repo.movedTo)
}

func TestMoveRefusesANonNumericEntityID(t *testing.T) {
	require.Equal(t, http.StatusBadRequest, serve(&fakeRepo{}, http.MethodPut, "/p/7/skill/items/abc", `{}`).Code)
	require.Equal(t, http.StatusBadRequest, serve(&fakeRepo{}, http.MethodPut, "/p/7/skill/items/0", `{}`).Code)
}

func TestDeleteAndErrorsMapToStatus(t *testing.T) {
	repo := &fakeRepo{}
	require.Equal(t, http.StatusNoContent, serve(repo, http.MethodDelete, "/p/7/skill/3", "").Code)
	require.Equal(t, "3", repo.deleted)

	missing := &fakeRepo{err: apierr.NotFound("folder not found")}
	require.Equal(t, http.StatusNotFound, serve(missing, http.MethodDelete, "/p/7/skill/3", "").Code)
	dup := &fakeRepo{err: apierr.Conflict("exists")}
	require.Equal(t, http.StatusConflict, serve(dup, http.MethodPost, "/p/7/skill", `{"name":"A"}`).Code)
}

func TestListReturnsItems(t *testing.T) {
	repo := &fakeRepo{list: []entityfolders.Folder{{ID: "1", Name: "A", ItemIDs: []int{4, 5}}}}
	rec := serve(repo, http.MethodGet, "/p/7/skill", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"item_ids":[4,5]`)
}

func TestAnUnknownKindIsNotFound(t *testing.T) {
	repo := &fakeRepo{}
	for _, path := range []string{"/p/7/widget", "/p/7/AGENT", "/p/7/applications"} {
		require.Equal(t, http.StatusNotFound, serve(repo, http.MethodGet, path, "").Code, path)
		require.Equal(t, http.StatusNotFound, serve(repo, http.MethodPost, path, `{"name":"x"}`).Code, path)
	}
	require.Empty(t, repo.createName)
}

func TestEveryKindIsServedWithItsOwnKind(t *testing.T) {
	for _, kind := range entityfolders.Kinds {
		repo := &fakeRepo{}
		rec := serve(repo, http.MethodPost, "/p/7/"+string(kind), `{"name":"A"}`)
		require.Equal(t, http.StatusCreated, rec.Code, kind)
		require.Equal(t, kind, repo.kind)
	}
}

func TestReorderWithUnknownNeighboursIsRefusedNotPlacedAtTheDefault(t *testing.T) {
	repo := &fakeRepo{list: []entityfolders.Folder{{ID: "1", Position: 2 * folders.PositionGap}, {ID: "3", Position: folders.PositionGap}}}
	for _, body := range []string{
		`{"neighbor_above_id":"99","neighbor_below_id":"1"}`,
		`{"neighbor_above_id":"1","neighbor_below_id":"nope"}`,
		`{"neighbor_below_id":"3"}`, // the folder cannot be its own neighbour
	} {
		rec := serve(repo, http.MethodPut, "/p/7/skill/3", body)
		require.Equal(t, http.StatusBadRequest, rec.Code, body)
	}
	require.Nil(t, repo.updPos, "nothing may be written for a refused reorder")
	require.Zero(t, repo.rebalances)
}

func TestReorderOfAMissingFolderIs404BeforeAnyRebalance(t *testing.T) {
	repo := &fakeRepo{list: []entityfolders.Folder{{ID: "1", Position: 11}, {ID: "2", Position: 10}}}
	rec := serve(repo, http.MethodPut, "/p/7/skill/77", `{"neighbor_above_id":"1","neighbor_below_id":"2"}`)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Zero(t, repo.rebalances)
}

func TestOutOfRangeIdsAndPositionsAre400(t *testing.T) {
	require.Equal(t, http.StatusBadRequest, serve(&fakeRepo{}, http.MethodPut, "/p/7/skill/items/2147483648", `{}`).Code)
	require.Equal(t, http.StatusBadRequest, serve(&fakeRepo{}, http.MethodPost, "/p/7/skill", `{"name":"A","position":2147483648}`).Code)
	require.Equal(t, http.StatusBadRequest, serve(&fakeRepo{}, http.MethodPut, "/p/7/skill/3", `{"position":-2147483649}`).Code)
	require.Equal(t, http.StatusNoContent, serve(&fakeRepo{}, http.MethodPut, "/p/7/skill/items/2147483647", `{}`).Code)
}

func TestCreateBodyIsCappedAtOneMebibyte(t *testing.T) {
	huge := `{"name":"` + strings.Repeat("x", 2<<20) + `"}`
	repo := &fakeRepo{}
	require.Equal(t, http.StatusBadRequest, serve(repo, http.MethodPost, "/p/7/skill", huge).Code)
	require.Empty(t, repo.createName)
}
