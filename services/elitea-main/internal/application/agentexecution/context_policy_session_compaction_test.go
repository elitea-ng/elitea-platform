package agentexecution

import (
	"encoding/json"
	"testing"
)

// The version's session_compaction (#1368) reaches the worker as it admits
// it; a malformed value runs on the defaults, as on the desktop.
func TestVersionSessionCompaction(t *testing.T) {
	for name, test := range map[string]struct {
		version string
		want    string
	}{
		"absent":        {`{"tools": []}`, ""},
		"no settings":   {`{"context_settings": {"enabled": true}}`, ""},
		"null":          {`{"context_settings": {"session_compaction": null}}`, ""},
		"on":            {`{"context_settings": {"session_compaction": true}}`, `true`},
		"off":           {`{"context_settings": {"session_compaction": false}}`, `false`},
		"tuned":         {`{"context_settings": {"session_compaction": {"keep_recent_tokens": 5000, "fresh_window": false}}}`, `{"fresh_window":false,"keep_recent_tokens":5000}`},
		"unknown key":   {`{"context_settings": {"session_compaction": {"other": 1}}}`, `true`},
		"zero tokens":   {`{"context_settings": {"session_compaction": {"reserve_tokens": 0}}}`, `true`},
		"quoted tokens": {`{"context_settings": {"session_compaction": {"reserve_tokens": "5000"}}}`, `true`},
		"too many":      {`{"context_settings": {"session_compaction": {"reserve_tokens": 4294967296}}}`, `true`},
		"a string":      {`{"context_settings": {"session_compaction": "yes"}}`, `true`},
		// A null field is that field's default, as the worker admits it.
		"null fresh":   {`{"context_settings": {"session_compaction": {"fresh_window": null, "keep_recent_tokens": 5000}}}`, `{"keep_recent_tokens":5000}`},
		"null reserve": {`{"context_settings": {"session_compaction": {"reserve_tokens": null, "fresh_window": false}}}`, `{"fresh_window":false}`},
		"all null":     {`{"context_settings": {"session_compaction": {"reserve_tokens": null, "keep_recent_tokens": null, "fresh_window": null}}}`, `{}`},
	} {
		t.Run(name, func(t *testing.T) {
			if got := string(versionSessionCompaction(json.RawMessage(test.version))); got != test.want {
				t.Fatalf("versionSessionCompaction = %q, want %q", got, test.want)
			}
		})
	}
}
