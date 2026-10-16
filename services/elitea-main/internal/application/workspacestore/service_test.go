package workspacestore

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestAllowedRefsPerWriterAndState(t *testing.T) {
	for _, tc := range []struct {
		writer   Writer
		state    State
		upstream bool
		want     []string
	}{
		{WriterDesktop, StateHandingOff, false, []string{RefBase, RefHandoff1}},
		{WriterDesktop, StateHandingOff, true, []string{RefBase, RefUpstream, RefHandoff1}},
		{WriterCloudExecutor, StateCloudRunning, true, []string{RefCloud}},
		{WriterDesktop, StateCloudIdle, true, nil},
		{WriterDesktop, StateCloudRunning, true, nil},
		{WriterCloudExecutor, StateCloudIdle, true, nil},
		{WriterCloudExecutor, StateHandingOff, true, nil},
		{WriterDesktop, StateReclaiming, true, nil},
		{WriterDesktop, StateLocal, true, nil},
	} {
		if got := AllowedRefs(tc.writer, tc.state, tc.upstream); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("AllowedRefs(%s, %s, %t) = %v, want %v", tc.writer, tc.state, tc.upstream, got, tc.want)
		}
	}
}

func TestValidatePublish(t *testing.T) {
	sha := strings.Repeat("a", 64)
	commit := strings.Repeat("b", 40)
	valid := PublishRequest{
		Thread: Thread{ProjectID: 1, ConversationUUID: "0b6f9a52-1c2d-4e5f-8a9b-0c1d2e3f4a5b"},
		Caller: Caller{Writer: WriterDesktop, UserID: 3}, Epoch: 1, Kind: KindCompact, SHA256: sha, Bytes: 10,
		Refs: []RefUpdate{{Name: RefBase, New: commit}}, ObjectFormat: ObjectFormatSHA1,
	}
	if err := validatePublish(valid); err != nil {
		t.Fatalf("valid publish refused: %v", err)
	}
	mutate := func(change func(*PublishRequest)) PublishRequest {
		request := valid
		request.Refs = append([]RefUpdate(nil), valid.Refs...)
		change(&request)
		return request
	}
	for name, tc := range map[string]struct {
		request PublishRequest
		want    error
	}{
		"sha256 repository": {mutate(func(r *PublishRequest) { r.ObjectFormat = "sha256" }), ErrUnsupportedObjectFormat},
		"PUSH in phase 1":   {mutate(func(r *PublishRequest) { r.Kind = "PUSH" }), ErrInvalid},
		"prerequisites":     {mutate(func(r *PublishRequest) { r.Prerequisites = []string{commit} }), ErrInvalid},
		"heads ref":         {mutate(func(r *PublishRequest) { r.Refs = []RefUpdate{{Name: "refs/heads/main", New: commit}} }), ErrInvalid},
		"duplicate ref": {mutate(func(r *PublishRequest) {
			r.Refs = []RefUpdate{{Name: RefBase, New: commit}, {Name: RefBase, New: commit}}
		}), ErrInvalid},
		"bad new sha":    {mutate(func(r *PublishRequest) { r.Refs[0].New = "HEAD" }), ErrInvalid},
		"bad digest":     {mutate(func(r *PublishRequest) { r.SHA256 = "x" }), ErrInvalid},
		"no refs":        {mutate(func(r *PublishRequest) { r.Refs = nil }), ErrInvalid},
		"zero epoch":     {mutate(func(r *PublishRequest) { r.Epoch = 0 }), ErrInvalid},
		"no executor id": {mutate(func(r *PublishRequest) { r.Caller = Caller{Writer: WriterCloudExecutor} }), ErrInvalid},
	} {
		if err := validatePublish(tc.request); !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v, want %v", name, err, tc.want)
		}
	}
}

func TestObjectKeyLayout(t *testing.T) {
	const conversation = "0b6f9a52-1c2d-4e5f-8a9b-0c1d2e3f4a5b"
	sha := strings.Repeat("c", 64)
	if got := ObjectKey(conversation, sha); got != "t/"+conversation+"/seg/"+sha+".seg" {
		t.Fatalf("ObjectKey = %s", got)
	}
	if got, ok := conversationOfKey(ObjectKey(conversation, sha)); !ok || got != conversation {
		t.Fatalf("conversationOfKey = %s, %t", got, ok)
	}
}
