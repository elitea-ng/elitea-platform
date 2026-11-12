package agentexecution

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

// #977 — `user_ids` is the mention list, no longer a parity gate.
func TestParseMentionedUserIDs(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		snake, camel string
		want         []int64
		ok           bool
	}{
		"absent on both is an ordinary message": {"", "", []int64{}, true},
		"snake_case alone":                      {`[11,12]`, "", []int64{11, 12}, true},
		// The spelling the composer actually sends. Before #977 this field was
		// not bound at all, so a real mention was silently dropped.
		"camelCase alone":               {"", `[11,12]`, []int64{11, 12}, true},
		"both, merged and deduplicated": {`[11]`, `[11,12]`, []int64{11, 12}, true},
		"explicit null is absent":       {`null`, `null`, []int64{}, true},
		"an empty array names nobody":   {`[]`, "", []int64{}, true},
		// A client that thinks it tagged somebody and did not is worth a 400:
		// the alternative is an admitted turn whose sender believes a colleague
		// was told.
		"a non-array is refused":       {`{"a":1}`, "", nil, false},
		"a non-positive id is refused": {`[0]`, "", nil, false},
		"a negative id is refused":     {`[-3]`, "", nil, false},
		"a non-numeric id is refused":  {`["11"]`, "", nil, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseMentionedUserIDs(json.RawMessage(tc.snake), json.RawMessage(tc.camel))
			if ok != tc.ok {
				t.Fatalf("parseMentionedUserIDs(%q,%q) ok = %v, want %v", tc.snake, tc.camel, ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseMentionedUserIDs(%q,%q) = %v, want %v", tc.snake, tc.camel, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseMentionedUserIDs(%q,%q) = %v, want %v", tc.snake, tc.camel, got, tc.want)
				}
			}
		})
	}
}

// The ceiling exists because the list is attacker-influenced and every entry
// becomes a row.
func TestParseMentionedUserIDsRefusesAnUnboundedAudience(t *testing.T) {
	t.Parallel()

	ids := make([]string, 0, maxMentionedUsers+1)
	for i := 1; i <= maxMentionedUsers+1; i++ {
		ids = append(ids, strconv.Itoa(i))
	}
	if _, ok := parseMentionedUserIDs(json.RawMessage("["+strings.Join(ids, ",")+"]"), nil); ok {
		t.Fatalf("a mention list past %d entries must be refused", maxMentionedUsers)
	}
}

// A mention with no addressed agent is a message for those people alone: the
// ad-hoc contract does not start a model turn for it either, and the people it
// names reach the use case that stores the message and notifies them.
func TestCurrentAdhocStartRoutePostsAPeopleOnlyMessageWithoutATurn(t *testing.T) {
	useCase := &currentStartUseCaseStub{}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	body := strings.Replace(validCurrentAdhocStartBody(), `"project_id":7,`,
		`"project_id":7,"user_ids":[12,13,12],"is_mentioning_everyone":true,`, 1)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentAdhocStartRequest(body))

	if response.Code != http.StatusCreated || useCase.postedCalls != 1 || useCase.adhocCalls != 0 || useCase.calls != 0 {
		t.Fatalf("status=%d posted=%d adhoc=%d app=%d body=%s", response.Code, useCase.postedCalls, useCase.adhocCalls, useCase.calls, response.Body.String())
	}
	if got := useCase.postedRequest.MentionedUserIDs; !reflect.DeepEqual(got, []int64{12, 13}) {
		t.Fatalf("mentions = %v, want the deduplicated [12 13]", got)
	}
	if !useCase.postedRequest.MentionsEveryone {
		t.Fatal("is_mentioning_everyone was dropped")
	}
	var answer map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if answer["turn_started"] != false || answer["created"] != true || answer["message_id"] != "ee92ccbd-3312-4c72-b20b-fddf224e7c0e" {
		t.Fatalf("answer = %v", answer)
	}
	if _, has := answer["events_url"]; has {
		t.Fatalf("a message with no turn has no events_url: %v", answer)
	}
}

// The people-only decision is made on the mentions and the addressed agents
// alone: a participant_id (what a client fills in for any unaddressed message)
// and the application contract do not start the picked or last-addressed
// agent, and a message with no mention is unchanged.
func TestCurrentApplicationStartRouteDecidesWhoAnswersFromTheMentionsAndAddressees(t *testing.T) {
	for _, tc := range []struct {
		name       string
		extra      string
		wantPosted bool
		wantTurn   bool
	}{
		{name: "people only, with a sticky participant", extra: `"user_ids":[12],`, wantPosted: true},
		{name: "camelCase mention list", extra: `"userIds":[12],`, wantPosted: true},
		{name: "everyone", extra: `"is_mentioning_everyone":true,`, wantPosted: true},
		{name: "people and an addressed agent", extra: `"user_ids":[12],"addressed_to":[21],`, wantTurn: true},
		{name: "no mention", extra: ``, wantTurn: true},
		{name: "an empty mention list", extra: `"user_ids":[],`, wantTurn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useCase := &currentStartUseCaseStub{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
				ExecutionID: "execution-1", CommandID: "command-1", ResponseMessageID: "response-1", Created: true,
			}}
			route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
			body := strings.Replace(validCurrentStartBody(), `"project_id":7,`, `"project_id":7,`+tc.extra, 1)

			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentStartRequest(body))

			if (useCase.postedCalls == 1) != tc.wantPosted || (useCase.calls == 1) != tc.wantTurn {
				t.Fatalf("posted=%d turns=%d body=%s", useCase.postedCalls, useCase.calls, response.Body.String())
			}
			if tc.wantTurn {
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "events_url") {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				if strings.Contains(tc.extra, "addressed_to") && !reflect.DeepEqual(useCase.request.MentionedUserIDs, []int64{12}) {
					t.Fatalf("a mixed message still notifies its people: %v", useCase.request.MentionedUserIDs)
				}
			}
		})
	}
}

func TestCurrentApplicationStartRouteRefusesAPeopleOnlyMessageTheServiceCannotStore(t *testing.T) {
	useCase := &currentStartUseCaseStub{err: agentexecutionapp.ErrPeopleMessagesUnavailable}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	body := strings.Replace(validCurrentStartBody(), `"project_id":7,`, `"project_id":7,"user_ids":[12],`, 1)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentStartRequest(body))

	if response.Code != http.StatusUnprocessableEntity || useCase.calls != 0 {
		t.Fatalf("status=%d turns=%d body=%s", response.Code, useCase.calls, response.Body.String())
	}
}

// Only people who would really be told make a message people-only: when none of
// the tagged people is a member other than the sender, the message is ordinary.
func TestCurrentApplicationStartRouteTreatsAMessageToNoRealPeopleAsOrdinary(t *testing.T) {
	useCase := &currentStartUseCaseStub{noRealAudience: true, outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "execution-1", CommandID: "command-1", ResponseMessageID: "response-1", Created: true,
	}}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	body := strings.Replace(validCurrentStartBody(), `"project_id":7,`, `"project_id":7,"user_ids":[11],`, 1)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentStartRequest(body))
	if response.Code != http.StatusOK || useCase.calls != 1 || useCase.postedCalls != 0 {
		t.Fatalf("status=%d turns=%d posted=%d body=%s", response.Code, useCase.calls, useCase.postedCalls, response.Body.String())
	}

	// A client that named no participant and no model has nobody to run it: stored, not refused.
	useCase = &currentStartUseCaseStub{noRealAudience: true}
	route = newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	bare := strings.Replace(body, `"participant_id":21,`, ``, 1)
	response = httptest.NewRecorder()
	route.ServeHTTP(response, currentStartRequest(bare))
	if response.Code != http.StatusCreated || useCase.postedCalls != 1 || useCase.calls != 0 {
		t.Fatalf("status=%d turns=%d posted=%d body=%s", response.Code, useCase.calls, useCase.postedCalls, response.Body.String())
	}
}

// The 201 names the run inputs no turn will read.
func TestPeopleOnlyAnswerNamesTheIgnoredRunInputs(t *testing.T) {
	useCase := &currentStartUseCaseStub{}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	body := strings.Replace(validCurrentAdhocStartBody(), `"project_id":7,`,
		`"project_id":7,"user_ids":[12],"unattended":{"sensitive_tools":[]},`, 1)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentAdhocStartRequest(body))
	var answer struct {
		Ignored []string `json:"ignored"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil || response.Code != http.StatusCreated {
		t.Fatalf("status=%d err=%v body=%s", response.Code, err, response.Body.String())
	}
	if !reflect.DeepEqual(answer.Ignored, []string{"llm_settings", "unattended"}) {
		t.Fatalf("ignored = %v", answer.Ignored)
	}

	plain := &currentStartUseCaseStub{}
	route = newCurrentStartRoute(t, plain, allowCurrentStartPermission())
	response = httptest.NewRecorder()
	route.ServeHTTP(response, currentStartRequest(strings.Replace(validCurrentStartBody(), `"project_id":7,`, `"project_id":7,"user_ids":[12],`, 1)))
	if strings.Contains(response.Body.String(), "ignored") {
		t.Fatalf("nothing was ignored: %s", response.Body.String())
	}
}
