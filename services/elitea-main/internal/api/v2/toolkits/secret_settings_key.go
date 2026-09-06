package toolkits

import "strings"

// WORDS, not substrings (#705).
//
// isSensitiveSettingKey used to answer with six strings.Contains calls:
//
//	strings.Contains(key, "secret") || strings.Contains(key, "token") ||
//	strings.Contains(key, "password") || strings.Contains(key, "credential") ||
//	strings.Contains(key, "api_key") || strings.Contains(key, "apikey")
//
// `max_tokens` contains `token`. So does `max_output_tokens`, `token_limit`
// and the `toolkit_configuration_` prefixed form of each. Every one of them
// was deleted from GET /elitea_core/tool/prompt_lib/{project}/{toolkit} and
// from the instance listing, so a client could not read back a value the user
// had saved. The DeepWiki chat and generation paths read
// `toolkit_configuration_max_tokens` out of the toolkit settings and fall back
// to 4096 when it is absent, so a project configured for 32000 silently ran at
// 4096 and the settings screen showed the field empty after a save that
// succeeded. Nothing logged, in either direction.
//
// The rule below matches whole WORDS. It is the Go half of the same
// correction apps/elitea-web/src/features/credentials/lib/schemaField.ts made
// for the credential form, and it uses the same three sets and the same
// last-word rule, so the two planes classify one key the same way. A key the
// form renders as a masked box is a key this redaction removes.
//
// The SDK toolkit schema snapshot is the AUDIT ORACLE for this rule, not an
// input to it. scripts/contract/sync_toolkit_schema_snapshot.py projects the
// SDK's `secret` annotation into
// internal/runtimecomposition/current_toolkit_schema_snapshot.json, and
// TestPinnedToolkitSnapshotSecretFieldsAreClassifiedSecret (in that package)
// fails when a field the SDK declares secret is not classified secret here.
// The snapshot cannot be the input: it carries the annotation for 11 fields
// across 52 toolkit types, and it names neither github's `access_token` nor
// jira's `password`, so a catalogue-only rule would under-redact.

// secretWholeKeys are key names that are secrets in their entirety. They are
// matched on the key with its separators removed, so `api_key`, `apiKey` and
// `apikey` are one entry.
var secretWholeKeys = map[string]struct{}{
	"apikey":        {},
	"apisecret":     {},
	"apitoken":      {},
	"authorization": {},
	"bearer":        {},
	"pat":           {},
}

// secretWords make the whole key a secret wherever they appear in it.
//
// `credential` and `credentials` are both listed because the substring rule
// this replaces caught both, and a settings field named `credentials` holds
// exactly what its name says.
var secretWords = map[string]struct{}{
	"credential":  {},
	"credentials": {},
	"passphrase":  {},
	"passwd":      {},
	"password":    {},
	"pwd":         {},
	"secret":      {},
}

// secretKeyQualifiers turn a trailing `key` into a credential. A bare `key` is
// deliberately not enough: `sort_key`, `partition_key` and `cache_key` are
// ordinary strings.
var secretKeyQualifiers = map[string]struct{}{
	"access":     {},
	"api":        {},
	"app":        {},
	"auth":       {},
	"client":     {},
	"consumer":   {},
	"encryption": {},
	"private":    {},
	"secret":     {},
	"service":    {},
	"signing":    {},
	"ssh":        {},
}

// settingsKeyWords splits a settings key into lower-case words on `_`, `-`,
// `.`, whitespace and camelCase boundaries. `maxOutputTokens` and
// `max_output_tokens` both become ["max", "output", "tokens"].
func settingsKeyWords(key string) []string {
	var words []string
	var current []rune
	runes := []rune(key)
	flush := func() {
		if len(current) > 0 {
			words = append(words, strings.ToLower(string(current)))
			current = current[:0]
		}
	}
	for index, letter := range runes {
		switch {
		case letter >= 'a' && letter <= 'z', letter >= '0' && letter <= '9':
			current = append(current, letter)
		case letter >= 'A' && letter <= 'Z':
			// A capital starts a new word only after a lower-case letter or a
			// digit. Runs of capitals stay together, so `APIKey` splits into
			// ["api", "key"] rather than ["a", "p", "i", "key"].
			if index > 0 {
				previous := runes[index-1]
				if (previous >= 'a' && previous <= 'z') ||
					(previous >= '0' && previous <= '9') {
					flush()
				}
			}
			current = append(current, letter)
		default:
			flush()
		}
	}
	flush()
	return words
}

// IsSecretSettingKey reports whether a toolkit settings key names a secret.
//
// `token` counts only as the LAST word and only in the singular. That is what
// separates a credential (`api_token`, `access_token`, `token`) from a budget
// (`max_tokens`, `max_output_tokens`) or a bound (`token_limit`).
//
// It is exported so the pinned SDK snapshot can be swept against it; see the
// file comment above.
func IsSecretSettingKey(key string) bool {
	words := settingsKeyWords(key)
	if len(words) == 0 {
		return false
	}
	if _, found := secretWholeKeys[strings.Join(words, "")]; found {
		return true
	}
	for _, word := range words {
		if _, found := secretWords[word]; found {
			return true
		}
	}
	last := words[len(words)-1]
	if last == "token" {
		return true
	}
	if last == "key" && len(words) > 1 {
		if _, found := secretKeyQualifiers[words[len(words)-2]]; found {
			return true
		}
	}
	return false
}
