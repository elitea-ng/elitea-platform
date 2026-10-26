package workspaceruntime

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateExecutorPublishTakesCompactAndPush(t *testing.T) {
	tip := strings.Repeat("a", 40)
	request := func(kind string, prerequisites ...string) PublishRequest {
		return PublishRequest{
			Kind: kind, SegmentSHA256: strings.Repeat("b", 64), Bytes: 10, Prerequisites: prerequisites,
			Refs:         []RefUpdate{{Name: RefCloud, Old: &tip, New: strings.Repeat("c", 40)}},
			ObjectFormat: ObjectFormatSHA1,
		}
	}
	for _, tc := range []struct {
		name    string
		request PublishRequest
		valid   bool
	}{
		{"compact", request(KindCompact), true},
		{"push on the old cloud tip", request(KindPush, tip), true},
		{"push without prerequisites", request(KindPush), false},
		{"compact with prerequisites", request(KindCompact, tip), false},
		{"push with a bad prerequisite", request(KindPush, "xyz"), false},
		{"push with a prerequisite twice", request(KindPush, tip, tip), false},
		{"another kind", request("DELTA"), false},
	} {
		err := ValidateExecutorPublish(tc.request)
		if (err == nil) != tc.valid {
			t.Errorf("%s: err = %v, want valid %t", tc.name, err, tc.valid)
		}
		if !tc.valid && !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", tc.name, err)
		}
	}
}
