package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestPeopleOnlyDecision(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		mentioned []int64
		everyone  bool
		addressed []int64
		want      bool
	}{
		{name: "a person", mentioned: []int64{11}, want: true},
		{name: "everyone", everyone: true, want: true},
		{name: "people and everyone", mentioned: []int64{11}, everyone: true, want: true},
		{name: "a person and an addressed agent", mentioned: []int64{11}, addressed: []int64{21}},
		{name: "everyone and an addressed agent", everyone: true, addressed: []int64{21}},
		{name: "no mention"},
		{name: "no mention, an addressed agent", addressed: []int64{21}},
	} {
		if got := PeopleOnly(tc.mentioned, tc.everyone, tc.addressed); got != tc.want {
			t.Errorf("%s: PeopleOnly = %v, want %v", tc.name, got, tc.want)
		}
	}
}

type recordingHumanMessages struct {
	posted  []HumanMessage
	created bool
	err     error
}

func (store *recordingHumanMessages) PostHumanMessage(_ context.Context, message HumanMessage) (bool, error) {
	store.posted = append(store.posted, message)
	return store.created, store.err
}

type peopleMentionWriter struct {
	members []int64
	rows    []MentionNotification
}

func (writer *peopleMentionWriter) WriteChatMentionNotifications(_ context.Context, rows []MentionNotification) error {
	writer.rows = append(writer.rows, rows...)
	return nil
}

func (writer *peopleMentionWriter) ProjectMemberUserIDs(context.Context, int64) ([]int64, error) {
	return writer.members, nil
}

func peopleRequest() CurrentApplicationStartRequest {
	return CurrentApplicationStartRequest{
		ProjectID: 7, ActorUserID: 7,
		ConversationUUID: "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		QuestionID:       "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
		UserInput:        "@Aksana can you see this?",
		MentionedUserIDs: []int64{11, 12},
	}
}

// A people-only message is stored as the sender's message with its mentions
// and notifies the people it names; nothing in this path can start a turn (the
// service has no resolver, admission or floor to call).
func TestPostPeopleMessageStoresTheMessageAndNotifiesThePeople(t *testing.T) {
	t.Parallel()
	store := &recordingHumanMessages{created: true}
	writer := &peopleMentionWriter{members: []int64{7, 11}}
	service := (&CurrentApplicationStartService{}).WithHumanMessages(store).WithMentionNotifications(writer)

	posted, err := service.PostPeopleMessage(context.Background(), peopleRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !posted.Created || posted.MessageID != peopleRequest().QuestionID {
		t.Fatalf("posted = %+v", posted)
	}
	if len(store.posted) != 1 || store.posted[0].UserInput != peopleRequest().UserInput || store.posted[0].AuthorUserID != 7 {
		t.Fatalf("stored = %+v", store.posted)
	}
	var meta struct {
		Mentioned []int64 `json:"mentioned_user_ids"`
	}
	if err := json.Unmarshal(store.posted[0].Meta, &meta); err != nil || len(meta.Mentioned) != 2 {
		t.Fatalf("meta = %s (%v)", store.posted[0].Meta, err)
	}
	// 12 is not a member and 7 is the sender: one notification, to 11.
	if len(writer.rows) != 1 || writer.rows[0].UserID != 11 || writer.rows[0].MessageID != peopleRequest().QuestionID {
		t.Fatalf("notifications = %+v", writer.rows)
	}
}

func TestPostPeopleMessageReplayNotifiesNobodyAgain(t *testing.T) {
	t.Parallel()
	store := &recordingHumanMessages{created: false}
	writer := &peopleMentionWriter{members: []int64{7, 11}}
	service := (&CurrentApplicationStartService{}).WithHumanMessages(store).WithMentionNotifications(writer)

	posted, err := service.PostPeopleMessage(context.Background(), peopleRequest())
	if err != nil || posted.Created {
		t.Fatalf("posted = %+v err = %v", posted, err)
	}
	if len(writer.rows) != 0 {
		t.Fatalf("a replay notified %+v", writer.rows)
	}
}

func TestPostPeopleMessageRefusesWhatIsNotPeopleOnly(t *testing.T) {
	t.Parallel()
	service := (&CurrentApplicationStartService{}).WithHumanMessages(&recordingHumanMessages{created: true})

	addressed := peopleRequest()
	addressed.AddressedTo = []int64{21}
	if _, err := service.PostPeopleMessage(context.Background(), addressed); !errors.Is(err, ErrInvalidCurrentAgentStart) {
		t.Fatalf("a message that addresses an agent is a turn, err = %v", err)
	}
	none := peopleRequest()
	none.MentionedUserIDs = nil
	if _, err := service.PostPeopleMessage(context.Background(), none); !errors.Is(err, ErrInvalidCurrentAgentStart) {
		t.Fatalf("a message with no mention is a turn, err = %v", err)
	}
}

func TestPostPeopleMessageWithoutAStoreStartsNothing(t *testing.T) {
	t.Parallel()
	_, err := (&CurrentApplicationStartService{}).PostPeopleMessage(context.Background(), peopleRequest())
	if !errors.Is(err, ErrPeopleMessagesUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestPostPeopleMessageStoreFailureNotifiesNobody(t *testing.T) {
	t.Parallel()
	store := &recordingHumanMessages{err: ErrUnsupportedCurrentAgentStart}
	writer := &peopleMentionWriter{members: []int64{7, 11}}
	service := (&CurrentApplicationStartService{}).WithHumanMessages(store).WithMentionNotifications(writer)
	if _, err := service.PostPeopleMessage(context.Background(), peopleRequest()); !errors.Is(err, ErrUnsupportedCurrentAgentStart) {
		t.Fatalf("err = %v", err)
	}
	if len(writer.rows) != 0 {
		t.Fatalf("notified %+v", writer.rows)
	}
}

func TestIsPeopleOnlyUsesTheEffectiveAudience(t *testing.T) {
	t.Parallel()
	writer := &peopleMentionWriter{members: []int64{7, 11}}
	service := (&CurrentApplicationStartService{}).WithMentionNotifications(writer)
	request := peopleRequest() // sender 7 tags 11 and 12
	if !service.IsPeopleOnly(context.Background(), request) {
		t.Fatal("a member other than the sender is somebody to talk to")
	}
	request.MentionedUserIDs = []int64{7, 12} // only the sender, and a non-member
	if service.IsPeopleOnly(context.Background(), request) {
		t.Fatal("nobody would be told: an ordinary message")
	}
	request.MentionedUserIDs = nil
	request.MentionsEveryone = true
	if !service.IsPeopleOnly(context.Background(), request) {
		t.Fatal("@everyone of a project with other members is people-only")
	}
	alone := (&CurrentApplicationStartService{}).WithMentionNotifications(&peopleMentionWriter{members: []int64{7}})
	if alone.IsPeopleOnly(context.Background(), request) {
		t.Fatal("@everyone with nobody else is an ordinary message")
	}
	request.AddressedTo = []int64{21}
	if service.IsPeopleOnly(context.Background(), request) {
		t.Fatal("an addressed agent keeps the turn")
	}
}
