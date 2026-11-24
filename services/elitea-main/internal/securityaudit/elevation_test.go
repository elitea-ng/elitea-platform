package securityaudit

// admin.elevation (ADR-0059 R2.3, issue 2517).

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const sampleElevationID = "0b7c2d4e-0000-4000-8000-000000000001"

// sampleElevationChange is a valid step of each action.
func sampleElevationChange(action ElevationAction) ElevationChange {
	change := ElevationChange{
		Action: action, Source: ElevationSourceConsole, ElevationID: sampleElevationID, UserID: 42,
		Level: "admin", Path: "approved", ReasonCode: "incident",
	}
	switch action {
	case ElevationEnded:
		change.EndReason = "holder"
	case ElevationExpired:
		change.EndReason, change.Source = "expired", ElevationSourceSystem
	case ElevationNoteAdded:
		change.NoteID, change.NoteLength = "note-7", 12
	case ElevationCredentialIssued, ElevationCredentialRevoked:
		change = ElevationChange{Action: action, Source: ElevationSourceOperator, CredentialID: "cred-3", Level: "super_admin"}
	case ElevationCredentialUsed:
		change.CredentialID, change.Path = "cred-3", "sealed"
	case ElevationLocalAdminSignin:
		change = ElevationChange{Action: action, Source: ElevationSourceConsole, UserID: 42}
	}
	return change
}

func sampleElevationEvent(change ElevationChange) SecurityEvent {
	event := NewAdminElevation(AdminElevation{ActorUserID: 7, Change: change})
	event.ID = "0192f0c1-7b6e-7c3a-8f00-00000000000a"
	event.Source = "elitea-main/test"
	event.Time = time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	return event
}

// Every action of the list builds a valid event whose canonical form carries
// the step under data.elevation.
func TestEveryElevationActionBuildsAValidEvent(t *testing.T) {
	for _, action := range ElevationActions() {
		t.Run(string(action), func(t *testing.T) {
			event := sampleElevationEvent(sampleElevationChange(action))
			if err := event.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			canonical, err := event.Canonical(1)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Data struct {
					Action    string        `json:"action"`
					Effect    string        `json:"effect"`
					Elevation elevationData `json:"elevation"`
				} `json:"data"`
			}
			if err := json.Unmarshal(canonical, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Data.Action != "admin.elevation."+string(action) || decoded.Data.Elevation.Action != string(action) {
				t.Fatalf("canonical action %q, step %q", decoded.Data.Action, decoded.Data.Elevation.Action)
			}
			wantEffect := string(EffectAllow)
			if action == ElevationDenied {
				wantEffect = string(EffectDeny)
			}
			if decoded.Data.Effect != wantEffect {
				t.Fatalf("effect %q, want %q", decoded.Data.Effect, wantEffect)
			}
			if (decoded.Data.Elevation.NoteLength != nil) != (action == ElevationNoteAdded) {
				t.Fatalf("note_length %v on %s", decoded.Data.Elevation.NoteLength, action)
			}
			if _, _, err := recanonicalize(canonical); err != nil {
				t.Fatalf("the stored form does not recanonicalize: %v", err)
			}
		})
	}
}

// The closed enum and the code fields refuse what is not on the list or not
// a code: no free string reaches the chain.
func TestAdminElevationRefusesWhatIsNotOnTheList(t *testing.T) {
	for name, mutate := range map[string]func(*SecurityEvent){
		"unknown action": func(e *SecurityEvent) {
			e.Elevation.Action = "extended"
			e.Action = "admin.elevation.extended"
		},
		"mismatched action":   func(e *SecurityEvent) { e.Action = "admin.elevation.ended" },
		"unknown source":      func(e *SecurityEvent) { e.Elevation.Source = "scim" },
		"free text as code":   func(e *SecurityEvent) { e.Elevation.ReasonCode = "the database is on fire" },
		"no elevation id":     func(e *SecurityEvent) { e.Elevation.ElevationID = "" },
		"malformed id":        func(e *SecurityEvent) { e.Elevation.ElevationID = "ELEVATION-1" },
		"end reason on start": func(e *SecurityEvent) { e.Elevation.EndReason = "holder" },
		"note on start":       func(e *SecurityEvent) { e.Elevation.NoteID, e.Elevation.NoteLength = "n", 3 },
		"credential on start": func(e *SecurityEvent) { e.Elevation.CredentialID = "cred-1" },
		"no payload":          func(e *SecurityEvent) { e.Elevation = nil },
		"payload on another":  func(e *SecurityEvent) { e.Type = TypeGrantChanged },
	} {
		t.Run(name, func(t *testing.T) {
			event := sampleElevationEvent(sampleElevationChange(ElevationStarted))
			mutate(&event)
			if err := event.Validate(); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("validate = %v, want ErrInvalidEvent", err)
			}
		})
	}
	ended := sampleElevationEvent(sampleElevationChange(ElevationEnded))
	ended.Elevation.EndReason = ""
	if err := ended.Validate(); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("ended without an end reason: %v", err)
	}
	note := sampleElevationEvent(sampleElevationChange(ElevationNoteAdded))
	note.Elevation.NoteLength = 0
	if err := note.Validate(); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("note_added without a length: %v", err)
	}
}

// The payload has no field that can hold a reason text or a note text, on
// the Go types and on the export schema, and the export schema names exactly
// the canonical form's keys and the action list: the export mapping goes with
// the type.
func TestElevationChangeHasNoFreeTextField(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeOf(ElevationChange{}), reflect.TypeOf(elevationData{}), reflect.TypeOf(AdminElevation{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			tag := strings.Split(field.Tag.Get("json"), ",")[0]
			if reasonTextName.MatchString(field.Name) || (tag != "" && reasonTextName.MatchString(tag)) ||
				strings.Contains(strings.ToLower(field.Name), "text") {
				t.Errorf("%s.%s can hold free text; an elevation step carries codes and ids only", typ.Name(), field.Name)
			}
		}
	}
	raw, err := os.ReadFile("../../../../libs/jsonschema/audit/v1/event.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties           map[string]json.RawMessage `json:"properties"`
			AdditionalProperties *bool                      `json:"additionalProperties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	step, ok := schema.Defs["elevationStep"]
	if !ok || len(step.Properties) == 0 {
		t.Fatal("the export schema has no elevationStep definition")
	}
	if step.AdditionalProperties == nil || *step.AdditionalProperties {
		t.Fatal("elevationStep must refuse additional properties, or a text could ride along")
	}
	typ := reflect.TypeOf(elevationData{})
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if _, ok := step.Properties[tag]; !ok {
			t.Errorf("the export schema's elevationStep lacks %q", tag)
		}
	}
	if len(step.Properties) != typ.NumField() {
		t.Errorf("the export schema's elevationStep has %d properties, the canonical form %d",
			len(step.Properties), typ.NumField())
	}
	var actions struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(step.Properties["action"], &actions); err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, action := range ElevationActions() {
		listed = append(listed, string(action))
	}
	if strings.Join(actions.Enum, ",") != strings.Join(listed, ",") {
		t.Errorf("the export schema lists the actions %v, the type %v", actions.Enum, listed)
	}
}
