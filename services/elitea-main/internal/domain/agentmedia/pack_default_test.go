package agentmedia

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestPackForAvatarKey(t *testing.T) {
	for _, name := range DefaultAvatarNames {
		pack := PackForAvatarKey(" " + DefaultAvatarPrefix + name + " ")
		if StillURL(pack) != DefaultAvatarPrefix+name {
			t.Fatalf("%s: pack = %s", name, pack)
		}
		// The stored pack passes the same check a later editor save runs.
		var decoded map[string]any
		if err := json.Unmarshal(pack, &decoded); err != nil {
			t.Fatal(err)
		}
		if _, err := NormalizePack(decoded); err != nil {
			t.Fatalf("%s: the stored pack is refused: %v", name, err)
		}
	}
	for _, key := range []string{"", "default:", "default:dragon", "fox", "other:fox", "DEFAULT:fox"} {
		if PackForAvatarKey(key) != nil {
			t.Fatalf("%q must map to no pack", key)
		}
	}
}

func TestDefaultStillIsForTheStillSlotOnly(t *testing.T) {
	_, err := NormalizePack(map[string]any{
		"schema": PackSchema,
		"slots": map[string]any{
			"still":    map[string]any{"url": "default:owl"},
			"thinking": map[string]any{"url": "default:owl"},
		},
	})
	if err == nil {
		t.Fatal("a default key in another slot must be refused")
	}
	if _, err := NormalizePack(map[string]any{
		"schema": PackSchema,
		"slots":  map[string]any{"still": map[string]any{"url": "default:nope"}},
	}); err == nil {
		t.Fatal("an unknown default name must be refused")
	}
}

// The Go names are the client set's: DEFAULT_AVATAR_NAMES in client-core.
func TestDefaultAvatarNamesMatchClientCore(t *testing.T) {
	source, err := os.ReadFile("../../../../../libs/ts/client-core/src/conversation/defaultAvatars.ts")
	if err != nil {
		t.Skipf("client-core is not in this checkout: %v", err)
	}
	match := regexp.MustCompile(`DEFAULT_AVATAR_NAMES = \[([^\]]*)\]`).FindStringSubmatch(string(source))
	if match == nil {
		t.Fatal("DEFAULT_AVATAR_NAMES not found in client-core")
	}
	var names []string
	for _, part := range strings.Split(match[1], ",") {
		if name := strings.Trim(strings.TrimSpace(part), "'\""); name != "" {
			names = append(names, name)
		}
	}
	if strings.Join(names, ",") != strings.Join(DefaultAvatarNames, ",") {
		t.Fatalf("client-core %v, Go %v", names, DefaultAvatarNames)
	}
}
