package eliteacore

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestDecodeProjectContextUpdateMatchesCurrentDefaultsAndActivationSemantics(t *testing.T) {
	update, err := decodeProjectContextUpdate(strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("decode defaults: %v", err)
	}
	if update.content != "" || !update.enabled || update.activationDescriptionSet || update.activationDescription != nil {
		t.Fatalf("defaults = %#v", update)
	}

	update, err = decodeProjectContextUpdate(strings.NewReader(
		`{"content":"rules","enabled":"false","activation_description":"  billing\n incidents  "}`,
	))
	if err != nil {
		t.Fatalf("decode explicit fields: %v", err)
	}
	if update.content != "rules" || update.enabled || !update.activationDescriptionSet ||
		update.activationDescription == nil || *update.activationDescription != "billing incidents" {
		t.Fatalf("explicit update = %#v", update)
	}

	for _, payload := range []string{
		`{"activation_description":null}`,
		`{"activation_description":"  "}`,
	} {
		update, err = decodeProjectContextUpdate(strings.NewReader(payload))
		if err != nil || !update.activationDescriptionSet || update.activationDescription != nil {
			t.Fatalf("removal %s = %#v, %v", payload, update, err)
		}
	}
}

func TestDecodeProjectContextUpdateRejectsInvalidAndOverlongFields(t *testing.T) {
	tests := []string{
		`null`,
		`[]`,
		`{} {}`,
		`{"content":null}`,
		`{"enabled":2}`,
		`{"activation_description":false}`,
		`{"content":"` + strings.Repeat("x", projectContextMaxContentRunes+1) + `"}`,
		`{"activation_description":"` + strings.Repeat("x", projectContextMaxActivationRunes+1) + `"}`,
	}
	for _, payload := range tests {
		if _, err := decodeProjectContextUpdate(strings.NewReader(payload)); err == nil {
			t.Fatalf("payload unexpectedly accepted: %.80s", payload)
		}
	}
}

func TestDecodeProjectContextDetailPreservesExactWireDefaults(t *testing.T) {
	updated := time.Date(2026, 9, 4, 12, 30, 45, 123456000, time.UTC)
	detail, err := decodeProjectContextDetail(17, []byte(`{"content":"rules"}`), &updated)
	if err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.ID == nil || *detail.ID != 17 || detail.Content != "rules" || !detail.Enabled ||
		detail.ActivationDescription != nil || detail.UpdatedAt == nil ||
		*detail.UpdatedAt != "2026-09-04T12:30:45.123456" {
		t.Fatalf("detail = %#v", detail)
	}

	encoded, err := json.Marshal(defaultProjectContextDetail())
	if err != nil || string(encoded) != `{"id":null,"content":"","enabled":true,"activation_description":null,"updated_at":null}` {
		t.Fatalf("default wire = %s, %v", encoded, err)
	}
}

func FuzzDecodeProjectContextUpdatePreservesBounds(f *testing.F) {
	for _, seed := range []string{
		`{}`,
		`{"content":"rules","enabled":false,"activation_description":"when relevant"}`,
		`{"activation_description":null}`,
		`{"enabled":"yes"}`,
		`not-json`,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, payload string) {
		update, err := decodeProjectContextUpdate(strings.NewReader(payload))
		if err != nil {
			return
		}
		if utf8.RuneCountInString(update.content) > projectContextMaxContentRunes {
			t.Fatalf("accepted content exceeds %d runes", projectContextMaxContentRunes)
		}
		if update.activationDescription != nil {
			if utf8.RuneCountInString(*update.activationDescription) > projectContextMaxActivationRunes {
				t.Fatalf("accepted activation description exceeds %d runes", projectContextMaxActivationRunes)
			}
			if *update.activationDescription != strings.Join(strings.Fields(*update.activationDescription), " ") {
				t.Fatalf("accepted activation description is not normalized: %q", *update.activationDescription)
			}
		}
	})
}
