package conversations

import (
	"encoding/json"
	"reflect"
	"testing"
)

func testConversation() Conversation {
	return Conversation{Source: EditorTestSource, Participants: []Participant{{EntityName: "application", EntityMeta: map[string]any{"id": "12", "project_id": "7"}, EntitySettings: map[string]any{"version_id": "19"}}}}
}
func TestEditorTestIdentityServerAuthority(t *testing.T) {
	identity, err := EditorTestIdentity(testConversation(), 41, "7")
	if err != nil || identity.ActorID != "41" || identity.ProjectID != "7" || identity.ApplicationID != "12" || identity.ApplicationVersionID != "19" {
		t.Fatalf("identity=%+v err=%v", identity, err)
	}
	cases := map[string]func(*Conversation){
		"actor spoof":         func(c *Conversation) { c.Meta = map[string]any{EditorTestSource: map[string]any{"actor_id": "99"}} },
		"foreign project":     func(c *Conversation) { c.Participants[0].EntityMeta["project_id"] = "8" },
		"missing version":     func(c *Conversation) { delete(c.Participants[0].EntitySettings, "version_id") },
		"fractional version":  func(c *Conversation) { c.Participants[0].EntitySettings["version_id"] = 1.5 },
		"extra user":          func(c *Conversation) { c.Participants = append(c.Participants, Participant{EntityName: "user"}) },
		"unsaved application": func(c *Conversation) { c.Participants[0].EntityMeta["id"] = "0" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c := testConversation()
			change(&c)
			if _, err := EditorTestIdentity(c, 41, "7"); err == nil {
				t.Fatal("accepted invalid identity")
			}
		})
	}
}
func TestEditorTestIdentityImmutableUpdate(t *testing.T) {
	current := testConversation()
	identity, _ := EditorTestIdentity(current, 41, "7")
	encoded, _ := json.Marshal(identity)
	var decoded any
	_ = json.Unmarshal(encoded, &decoded)
	current.Meta = map[string]any{EditorTestSource: decoded, "is_hidden": true, "single_participant": map[string]any{"id": "12"}}
	update := Conversation{Meta: map[string]any{"steps_limit": float64(10)}}
	if err := PreserveEditorTestUpdate(current, &update); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(update.Meta[EditorTestSource], decoded) || update.Meta["is_hidden"] != true {
		t.Fatal("reserved context lost")
	}
	for _, key := range []string{EditorTestSource, "single_participant", "is_hidden"} {
		update = Conversation{Meta: map[string]any{key: false}}
		if err := PreserveEditorTestUpdate(current, &update); err == nil {
			t.Fatalf("changed %s accepted", key)
		}
	}
	public := false
	update = Conversation{IsPrivate: &public}
	if err := PreserveEditorTestUpdate(current, &update); err == nil {
		t.Fatal("published Test accepted")
	}
	ordinary := Conversation{Source: "elitea"}
	update = Conversation{Meta: map[string]any{EditorTestSource: decoded}}
	if err := PreserveEditorTestUpdate(ordinary, &update); err == nil {
		t.Fatal("ordinary row acquired Test authority")
	}
	update = Conversation{Name: "renamed"}
	if err := PreserveEditorTestUpdate(ordinary, &update); err != nil {
		t.Fatal(err)
	}
}
