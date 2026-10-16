package testpg

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// databaseURLEnvName matches the environment variables a test reads a
// PostgreSQL URL from: ELITEA_TEST_DATABASE_URL, ELITEA_*_TEST_DATABASE_URL,
// ELITEA_SUBAPP_HOST_TEST_DSN, DATABASE_URL and so on.
var databaseURLEnvName = regexp.MustCompile(`^[A-Z0-9_]*(DATABASE_URL|_DSN)$`)

// notADatabaseURL are switches whose names end like a URL variable.
var notADatabaseURL = map[string]bool{"ELITEA_TEST_USE_SERVICE_DATABASE_URL": true}

var (
	envRead       = regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\(\s*([A-Za-z_][\w.]*|"[A-Z0-9_]+")\s*\)`)
	stringConst   = regexp.MustCompile(`\b([A-Za-z_]\w*)\s*(?:string\s*)?=\s*"([A-Z0-9_]+)"`)
	wrappedPrefix = "testpg.DSN("
)

// TestEveryDatabaseURLReadGoesThroughTestpg keeps the non-UTC session guard
// complete: a new test that reads a database URL without testpg.DSN would
// open a UTC session on CI and hide a naive-timestamp defect again.
func TestEveryDatabaseURLReadGoesThroughTestpg(t *testing.T) {
	root := moduleRoot(t)
	var offenders []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case "node_modules", "vendor", "testdata", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") && !isTestHelper(root, path) {
			return nil
		}
		if filepath.Base(filepath.Dir(path)) == "testpg" {
			return nil // this package reads the URL to test DSN itself
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(source)
		names := map[string]string{}
		for _, match := range stringConst.FindAllStringSubmatch(text, -1) {
			names[match[1]] = match[2]
		}
		for _, index := range envRead.FindAllStringSubmatchIndex(text, -1) {
			argument := text[index[2]:index[3]]
			name := strings.Trim(argument, `"`)
			if !strings.HasPrefix(argument, `"`) {
				name = names[argument[strings.LastIndex(argument, ".")+1:]]
				if name == "" {
					name = packageConstant(t, root, argument)
				}
			}
			if !databaseURLEnvName.MatchString(name) || notADatabaseURL[name] {
				continue
			}
			if strings.HasSuffix(text[:index[0]], wrappedPrefix) {
				continue
			}
			lineEnd := strings.IndexByte(text[index[1]:], '\n')
			if lineEnd < 0 {
				lineEnd = len(text) - index[1]
			}
			if strings.Contains(text[index[1]:index[1]+lineEnd], ApplyMarker) && strings.Contains(text, "testpg.Apply(") {
				continue
			}
			line := strings.Count(text[:index[0]], "\n") + 1
			relative, _ := filepath.Rel(root, path)
			offenders = append(offenders, relative+":"+itoa(line)+" reads "+name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("these reads of a database URL bypass testpg.DSN, so their sessions run in the server's time zone (UTC on CI):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// isTestHelper names the non-_test.go files that only tests import: packages
// whose name ends in "test" (authstatetest, dbtest, ...).
func isTestHelper(root, path string) bool {
	relative, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil {
		return false
	}
	return strings.HasSuffix(filepath.Base(relative), "test") && strings.HasSuffix(path, ".go")
}

var constantCache = map[string]string{}

// packageConstant resolves an identifier declared in another file of the
// same module, e.g. a package-level `const fooDatabaseURL = "..."`.
func packageConstant(t *testing.T, root, identifier string) string {
	t.Helper()
	identifier = identifier[strings.LastIndex(identifier, ".")+1:]
	if len(constantCache) == 0 {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			source, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			for _, match := range stringConst.FindAllStringSubmatch(string(source), -1) {
				if _, seen := constantCache[match[1]]; !seen {
					constantCache[match[1]] = match[2]
				}
			}
			return nil
		})
	}
	return constantCache[identifier]
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("no go.mod above the test directory")
		}
		directory = parent
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func TestDSNSetsTheSessionTimeZone(t *testing.T) {
	t.Setenv(TimeZoneEnv, "")
	cases := map[string]string{
		"": "",
		"postgres://u:p@h:5432/db?sslmode=disable": "postgres://u:p@h:5432/db?sslmode=disable&timezone=America%2FToronto",
		"postgres://u@h/db":                        "postgres://u@h/db?timezone=America%2FToronto",
		"postgres://u@h/db?timezone=UTC":           "postgres://u@h/db?timezone=UTC",
		"host=h dbname=db":                         "host=h dbname=db timezone='America/Toronto'",
		"host=h timezone=UTC":                      "host=h timezone=UTC",
	}
	for input, want := range cases {
		if got := DSN(input); got != want {
			t.Errorf("DSN(%q) = %q, want %q", input, got, want)
		}
	}
	t.Setenv(TimeZoneEnv, "Asia/Kolkata")
	if got := DSN("postgres://u@h/db"); got != "postgres://u@h/db?timezone=Asia%2FKolkata" {
		t.Errorf("override: DSN = %q", got)
	}
}
