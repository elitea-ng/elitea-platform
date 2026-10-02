package predict

import (
	"context"
	"errors"
	"reflect"
	"testing"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

type defaultCatalogStub struct {
	result configurationapp.CurrentModelCatalogResponse
	err    error
	query  configurationapp.CurrentModelCatalogQuery
	calls  int
}

func (s *defaultCatalogStub) Get(_ context.Context, query configurationapp.CurrentModelCatalogQuery) (configurationapp.CurrentModelCatalogResponse, error) {
	s.calls++
	s.query = query
	return s.result, s.err
}

func TestDefaultModelCompleter(t *testing.T) {
	name := "configured-model"
	local, public := int32(2), int32(1)
	for _, tc := range []struct {
		name      string
		explicit  string
		project   string
		owner     *int32
		items     []configurationapp.CurrentModelCatalogItem
		lookupErr error
		wantErr   error
	}{
		{name: "project default", project: "2", owner: &local, items: []configurationapp.CurrentModelCatalogItem{{Name: name, ProjectID: 2}}},
		{name: "shared default retains requester", project: "2", owner: &public, items: []configurationapp.CurrentModelCatalogItem{{Name: name, ProjectID: 1}}},
		{name: "explicit bypasses catalog", project: "2", explicit: "explicit-model"},
		{name: "missing default", project: "2", wantErr: ErrDefaultModelUnavailable},
		{name: "unavailable default", project: "2", owner: &public, items: []configurationapp.CurrentModelCatalogItem{{Name: name, ProjectID: 2}}, wantErr: ErrDefaultModelUnavailable},
		{name: "lookup failure", project: "2", lookupErr: context.DeadlineExceeded, wantErr: context.DeadlineExceeded},
		{name: "invalid project", project: "invalid", wantErr: ErrDefaultModelUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := &defaultCatalogStub{result: configurationapp.CurrentModelCatalogResponse{DefaultModelName: &name, DefaultModelProjectID: tc.owner, Items: tc.items}, err: tc.lookupErr}
			next := &recordingCompleter{content: "draft"}
			completer, err := WithDefaultModel(next, catalog, 1)
			if err != nil {
				t.Fatal(err)
			}
			temp := 0.0
			req := CompletionRequest{ProjectID: tc.project, UserID: "3", Model: tc.explicit, Messages: []Message{{Role: "user", Content: "draft only"}}, Temperature: &temp}
			result, err := completer.Complete(context.Background(), req)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || next.calls != 0 {
					t.Fatalf("err=%v calls=%d", err, next.calls)
				}
				return
			}
			if err != nil || result != "draft" || next.calls != 1 {
				t.Fatalf("result=%q err=%v calls=%d", result, err, next.calls)
			}
			want := req
			if tc.explicit == "" {
				want.Model = name
				expectedQuery := configurationapp.CurrentModelCatalogQuery{Section: configurationapp.CurrentModelSectionLLM, ProjectID: 2, PublicProjectID: 1, IncludeShared: true}
				if catalog.query != expectedQuery {
					t.Fatalf("query=%+v", catalog.query)
				}
			} else if catalog.calls != 0 {
				t.Fatal("explicit model queried catalog")
			}
			if !reflect.DeepEqual(next.got, want) {
				t.Fatalf("completion changed request: %+v", next.got)
			}
		})
	}
}
