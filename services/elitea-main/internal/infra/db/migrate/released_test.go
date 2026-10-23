package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"

	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
	"github.com/stretchr/testify/require"
)

// dollarTag matches a dollar-quote delimiter: $$ or $tag$, never a $1 parameter.
var dollarTag = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)?\$`)

// executableSQL is a migration's text with its comments removed and every run
// of whitespace outside a literal collapsed to one space: what PostgreSQL
// executes, so two texts that differ only in comments and wrapping give the
// same result. Literals ('…', E'…', "…") are kept byte for byte. A
// dollar-quote delimiter is kept but its body is read as SQL, so the comments
// inside a DO block or function body are removed too.
func executableSQL(sql string) string {
	var out strings.Builder
	space := false
	emit := func(s string) {
		if space && out.Len() > 0 {
			out.WriteByte(' ')
		}
		space = false
		out.WriteString(s)
	}
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			space = true
			i++
		case strings.HasPrefix(sql[i:], "--"):
			end := strings.IndexByte(sql[i:], '\n')
			if end < 0 {
				end = len(sql) - i
			}
			space = true
			i += end
		case strings.HasPrefix(sql[i:], "/*"):
			depth, j := 0, i
			for j < len(sql) {
				if strings.HasPrefix(sql[j:], "/*") {
					depth++
					j += 2
				} else if strings.HasPrefix(sql[j:], "*/") {
					depth--
					j += 2
					if depth == 0 {
						break
					}
				} else {
					j++
				}
			}
			space = true
			i = j
		case c == '\'' || c == '"':
			escapes := c == '\'' && i > 0 && (sql[i-1] == 'E' || sql[i-1] == 'e') &&
				(i < 2 || !isIdentByte(sql[i-2]))
			j := i + 1
			for j < len(sql) {
				if escapes && sql[j] == '\\' {
					j += 2
					continue
				}
				if sql[j] == c {
					if j+1 < len(sql) && sql[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			end := min(j+1, len(sql))
			emit(sql[i:end])
			i = end
		case c == '$' && (i == 0 || !isIdentByte(sql[i-1])) && dollarTag.MatchString(sql[i:]):
			tag := dollarTag.FindString(sql[i:])
			emit(tag)
			i += len(tag)
		default:
			j := i + 1
			for j < len(sql) && !strings.ContainsRune(" \t\n\r\f'\"$-/", rune(sql[j])) {
				j++
			}
			emit(sql[i:j])
			i = j
		}
	}
	return out.String()
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func executableDigest(sql []byte) string {
	sum := sha256.Sum256([]byte(executableSQL(string(sql))))
	return hex.EncodeToString(sum[:])
}

func TestExecutableSQLRemovesOnlyComments(t *testing.T) {
	t.Parallel()

	require.Equal(t,
		`SELECT 'a -- b', E'it\'s -- x', "c--d" FROM t WHERE x = $1; DO $body$ BEGIN PERFORM 1; END $body$;`,
		executableSQL("-- header\nSELECT 'a -- b', E'it\\'s -- x', \"c--d\"\n  FROM t /* a /* nested */ note */\n"+
			"WHERE x = $1;\nDO $body$\nBEGIN\n    -- inside the body\n    PERFORM 1;\nEND\n$body$;\n"))
	require.Equal(t, executableSQL("SELECT 1; -- one\n-- two\nSELECT 2;"), executableSQL("SELECT 1;\n-- one two\nSELECT 2;"))
	require.NotEqual(t, executableSQL("SELECT 'a b'"), executableSQL("SELECT 'a  b'"))
	require.NotEqual(t, executableSQL("SELECT a-b"), executableSQL("SELECT a - b -- x"))
}

// Every rewritten migration changed in its comments and nothing else: its
// executable text is the released one. And the current text is not itself
// listed, so a rewrite that was never needed cannot hide here.
func TestReleasedTextsDifferOnlyInComments(t *testing.T) {
	t.Parallel()

	for scope, versions := range releasedTexts {
		manifest, err := LoadManifest(platformmigrations.Files, scope)
		require.NoError(t, err)
		byVersion := make(map[int64]Migration, len(manifest))
		for _, migration := range manifest {
			byVersion[migration.Version] = migration
		}
		for version, text := range versions {
			migration, ok := byVersion[version]
			require.Truef(t, ok, "%s %04d is listed as rewritten but is not in the manifest", scope, version)
			require.Equalf(t, text.executable, executableDigest(migration.SQL),
				"%s changed outside its comments; the released text ran on deployed databases, so add a new migration instead", migration.Path)
			require.NotEqualf(t, text.checksum, hex.EncodeToString(migration.Checksum[:]),
				"%s is listed as rewritten but still has its released text", migration.Path)
		}
	}
}

func TestValidateRecordedLedgerAcceptsAReleasedText(t *testing.T) {
	t.Parallel()

	manifest, err := LoadManifest(platformmigrations.Files, ScopeShared)
	require.NoError(t, err)
	var migration Migration
	var text released
	for _, candidate := range manifest {
		if found, ok := releasedTexts[ScopeShared][candidate.Version]; ok {
			migration, text = candidate, found
			break
		}
	}
	require.NotZero(t, migration.Version, "no shared migration is listed as rewritten")
	recorded, err := hex.DecodeString(text.checksum)
	require.NoError(t, err)

	prefix := func(entry recordedMigration) []recordedMigration {
		var entries []recordedMigration
		for _, earlier := range manifest {
			if earlier.Version == migration.Version {
				return append(entries, entry)
			}
			entries = append(entries, recordedMigration{version: earlier.Version, name: earlier.Name, checksum: earlier.Checksum[:]})
		}
		return entries
	}

	// The released text under the name it was released with.
	_, err = validateRecordedLedger(manifest, prefix(recordedMigration{version: migration.Version, name: "released_name", checksum: recorded}), false)
	require.NoError(t, err)
	// The current text under its current name.
	_, err = validateRecordedLedger(manifest, prefix(recordedMigration{version: migration.Version, name: migration.Name, checksum: migration.Checksum[:]}), false)
	require.NoError(t, err)
	// The current text under another name is still refused, and so is any third text.
	_, err = validateRecordedLedger(manifest, prefix(recordedMigration{version: migration.Version, name: "other", checksum: migration.Checksum[:]}), false)
	require.ErrorContains(t, err, "name mismatch")
	other := sha256.Sum256([]byte("SELECT 1"))
	_, err = validateRecordedLedger(manifest, prefix(recordedMigration{version: migration.Version, name: migration.Name, checksum: other[:]}), false)
	require.ErrorContains(t, err, "checksum mismatch")
}
