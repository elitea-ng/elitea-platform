package testpg

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// databaseEnvName matches every environment variable a test could read a
// PostgreSQL location from: anything that names a DATABASE or a DSN
// (ELITEA_TEST_DATABASE_URL, ELITEA_SUBAPP_HOST_TEST_DSN, DATABASE_URL, ...),
// and libpq's own PGHOST/PGDATABASE.
var databaseEnvName = regexp.MustCompile(`DATABASE|DSN|^PGHOST$|^PGDATABASE$`)

// notADatabaseLocation are switches whose names match the filter but hold no
// connection string.
var notADatabaseLocation = map[string]bool{
	"ELITEA_TEST_USE_SERVICE_DATABASE_URL": true,
}

var (
	envRead     = regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\(`)
	stringConst = regexp.MustCompile(`\b([A-Za-z_]\w*)\s*(?:string\s*)?=\s*"([A-Za-z0-9_]+)"`)
	stringLit   = regexp.MustCompile(`^"([A-Za-z0-9_]+)"$`)
	identifier  = regexp.MustCompile(`^(?:[A-Za-z_]\w*\.)?([A-Za-z_]\w*)$`)
	parseInto   = regexp.MustCompile(`(\w+)\s*,\s*\w+\s*:?=\s*(?:pgxpool|pgx|pgconn)\.ParseConfig\(`)
	funcStart   = regexp.MustCompile(`(?m)^func\b`)
)

const wrappedPrefix = "testpg.DSN("

// TestEveryDatabaseURLReadGoesThroughTestpg keeps the non-UTC session guard
// complete: a test that opens a connection from the environment without
// testpg would run in the server's time zone (UTC on CI) and hide a
// naive-timestamp defect again.
//
// A read is accepted only when it is wrapped in testpg.DSN(...), or marked
// with ApplyMarker and the same function applies testpg.Apply to the
// RuntimeParams of the config it parses. A read whose argument is not a
// string literal or a constant this module declares (a helper parameter, a
// concatenation) is refused unless wrapped: its name cannot be checked.
func TestEveryDatabaseURLReadGoesThroughTestpg(t *testing.T) {
	root := moduleRoot(t)
	offenders := scanModule(t, root)
	if len(offenders) > 0 {
		t.Fatalf("these environment reads bypass testpg, so their sessions run in the server's time zone (UTC on CI):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// scanModule returns the offending reads under root.
func scanModule(t *testing.T, root string) []string {
	t.Helper()
	constants := moduleConstants(root)
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
		if !strings.HasSuffix(path, ".go") || (!strings.HasSuffix(path, "_test.go") && !isTestHelper(root, path)) {
			return nil
		}
		if filepath.Base(filepath.Dir(path)) == "testpg" && filepath.Dir(filepath.Dir(path)) == filepath.Join(root, "internal") {
			return nil // this package reads the URL to test DSN itself
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		offenders = append(offenders, scanSource(relative, string(source), constants)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return offenders
}

// scanSource returns the offending reads in one file.
func scanSource(name, text string, constants map[string]string) []string {
	local := map[string]string{}
	for _, match := range stringConst.FindAllStringSubmatch(text, -1) {
		local[match[1]] = match[2]
	}
	var offenders []string
	for _, location := range envRead.FindAllStringIndex(text, -1) {
		open := location[1] - 1
		closing := matchingParen(text, open)
		if closing < 0 {
			continue
		}
		argument := strings.TrimSpace(text[open+1 : closing])
		line := strings.Count(text[:location[0]], "\n") + 1
		where := name + ":" + strconv.Itoa(line)
		envName, resolved := resolveName(argument, local, constants)
		if resolved && (!databaseEnvName.MatchString(envName) || notADatabaseLocation[envName]) {
			continue
		}
		if strings.HasSuffix(text[:location[0]], wrappedPrefix) {
			continue
		}
		if !resolved {
			if markerReason(text, closing, NotADatabaseMarker) {
				continue
			}
			offenders = append(offenders, where+" reads "+argument+", which is not a literal or a constant: wrap it in testpg.DSN, or mark it "+NotADatabaseMarker+" with a reason")
			continue
		}
		if hasMarker(text, closing) {
			if appliedInSameFunction(text, location[0]) {
				continue
			}
			offenders = append(offenders, where+" reads "+envName+" marked "+ApplyMarker+", but its function never calls testpg.Apply on the parsed config's RuntimeParams")
			continue
		}
		offenders = append(offenders, where+" reads "+envName)
	}
	return offenders
}

func resolveName(argument string, local, constants map[string]string) (string, bool) {
	if match := stringLit.FindStringSubmatch(argument); match != nil {
		return match[1], true
	}
	if match := identifier.FindStringSubmatch(argument); match != nil {
		if value, ok := local[match[1]]; ok {
			return value, true
		}
		if value, ok := constants[match[1]]; ok {
			return value, true
		}
	}
	return "", false
}

// markerReason reports whether the line from `from` carries marker followed
// by a reason of at least a few words.
func markerReason(text string, from int, marker string) bool {
	end := strings.IndexByte(text[from:], '\n')
	if end < 0 {
		end = len(text) - from
	}
	line := text[from : from+end]
	index := strings.Index(line, marker)
	if index < 0 {
		return false
	}
	return len(strings.Fields(strings.Trim(line[index+len(marker):], " :-—()"))) >= 3
}

func hasMarker(text string, from int) bool {
	end := strings.IndexByte(text[from:], '\n')
	if end < 0 {
		end = len(text) - from
	}
	return strings.Contains(text[from:from+end], ApplyMarker)
}

// appliedInSameFunction reports whether the function holding offset parses a
// config into a variable and calls testpg.Apply on that variable's
// RuntimeParams after the read.
func appliedInSameFunction(text string, offset int) bool {
	start := 0
	for _, match := range funcStart.FindAllStringIndex(text, -1) {
		if match[0] > offset {
			break
		}
		start = match[0]
	}
	end := len(text)
	if next := funcStart.FindStringIndex(text[offset:]); next != nil {
		end = offset + next[0]
	}
	body := text[start:end]
	readAt := offset - start
	for _, parse := range parseInto.FindAllStringSubmatchIndex(body, -1) {
		if parse[0] < readAt {
			continue
		}
		variable := body[parse[2]:parse[3]]
		applied := regexp.MustCompile(`testpg\.Apply\(\s*` + regexp.QuoteMeta(variable) + `\.(?:ConnConfig\.)?RuntimeParams\s*\)`)
		if loc := applied.FindStringIndex(body[parse[1]:]); loc != nil {
			return true
		}
	}
	return false
}

func matchingParen(text string, open int) int {
	depth := 0
	inString := byte(0)
	for index := open; index < len(text); index++ {
		character := text[index]
		if inString != 0 {
			if character == '\\' && inString == '"' {
				index++
				continue
			}
			if character == inString {
				inString = 0
			}
			continue
		}
		switch character {
		case '"', '`', '\'':
			inString = character
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return index
			}
		}
	}
	return -1
}

// isTestHelper names the non-_test.go files that only tests import: packages
// whose name ends in "test" (authstatetest, dbtest, ...).
func isTestHelper(root, path string) bool {
	relative, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil {
		return false
	}
	return strings.HasSuffix(filepath.Base(relative), "test")
}

// moduleConstants maps every string constant the module declares to its value.
func moduleConstants(root string) map[string]string {
	constants := map[string]string{}
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for _, match := range stringConst.FindAllStringSubmatch(string(source), -1) {
			if _, seen := constants[match[1]]; !seen {
				constants[match[1]] = match[2]
			}
		}
		return nil
	})
	return constants
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

func itoa(value int) string { return strconv.Itoa(value) }

// Each case is a file the guard must refuse or accept. They are the scratch
// offenders that prove the guard catches every shape it claims to.
func TestTheGuardRefusesEveryBypass(t *testing.T) {
	constants := map[string]string{"sharedURLEnv": "ELITEA_TEST_DATABASE_URL"}
	cases := []struct {
		name   string
		source string
		refuse bool
	}{
		{"literal read", `func f() { _ = os.Getenv("ELITEA_TEST_DATABASE_URL") }`, true},
		{"wrapped literal", `func f() { _ = testpg.DSN(os.Getenv("ELITEA_TEST_DATABASE_URL")) }`, false},
		{"module constant", `func f() { _ = os.Getenv(sharedURLEnv) }`, true},
		{"package-qualified constant", `func f() { _ = os.Getenv(other.sharedURLEnv) }`, true},
		{"LookupEnv", `func f() { _, _ = os.LookupEnv("ELITEA_SUBAPP_HOST_TEST_DSN") }`, true},
		{"helper parameter", `func f(name string) { _ = os.Getenv(name) }`, true},
		{"wrapped helper parameter", `func f(name string) { _ = testpg.DSN(os.Getenv(name)) }`, false},
		{"concatenation", `func f() { _ = os.Getenv("ELITEA_" + "TEST_DATABASE_URL") }`, true},
		{"any DATABASE name", `func f() { _ = os.Getenv("MY_DATABASE") }`, true},
		{"any DSN name", `func f() { _ = os.Getenv("WAREHOUSE_DSN_PRIMARY") }`, true},
		{"PGHOST", `func f() { _ = os.Getenv("PGHOST") }`, true},
		{"PGDATABASE", `func f() { _ = os.Getenv("PGDATABASE") }`, true},
		{"unrelated literal", `func f() { _ = os.Getenv("ELITEA_TEST_NATS_URL") }`, false},
		{"dynamic name marked with a reason", "func f(key string) string {\n\treturn os.Getenv(key) // testpg:not-a-database: only the CONTRACT_* URLs reach this helper\n}", false},
		{"dynamic name marked without a reason", "func f(key string) string {\n\treturn os.Getenv(key) // testpg:not-a-database\n}", true},
		{"unrelated switch", `func f() { _ = os.Getenv("ELITEA_TEST_USE_SERVICE_DATABASE_URL") }`, false},
		{"marker without Apply", "func f() {\n\traw := os.Getenv(\"ELITEA_TEST_DATABASE_URL\") // testpg:apply\n\tconfig, _ := pgxpool.ParseConfig(raw)\n\t_ = config\n}", true},
		{"marker with Apply in another function", "func f() {\n\traw := os.Getenv(\"ELITEA_TEST_DATABASE_URL\") // testpg:apply\n\tconfig, _ := pgxpool.ParseConfig(raw)\n\t_ = config\n}\nfunc g() {\n\tother, _ := pgxpool.ParseConfig(\"x\")\n\ttestpg.Apply(other.ConnConfig.RuntimeParams)\n}", true},
		{"marker with Apply on another config", "func f() {\n\traw := os.Getenv(\"ELITEA_TEST_DATABASE_URL\") // testpg:apply\n\tconfig, _ := pgxpool.ParseConfig(raw)\n\tother, _ := pgxpool.ParseConfig(\"x\")\n\ttestpg.Apply(somethingElse.ConnConfig.RuntimeParams)\n\t_, _ = config, other\n}", true},
		{"marker with Apply on its config", "func f() {\n\traw := os.Getenv(\"ELITEA_TEST_DATABASE_URL\") // testpg:apply\n\tconfig, err := pgxpool.ParseConfig(raw)\n\t_ = err\n\ttestpg.Apply(config.ConnConfig.RuntimeParams)\n}", false},
	}
	for _, testCase := range cases {
		offenders := scanSource("scratch_test.go", "package scratch\n"+testCase.source+"\n", constants)
		if refused := len(offenders) > 0; refused != testCase.refuse {
			t.Errorf("%s: refused=%v, want %v (%v)", testCase.name, refused, testCase.refuse, offenders)
		}
	}
}

func TestDSNSetsTheSessionTimeZone(t *testing.T) {
	t.Setenv(TimeZoneEnv, "")
	cases := map[string]string{
		"": "",
		"postgres://u:p@h:5432/db?sslmode=disable":       "postgres://u:p@h:5432/db?options=-c%20TimeZone%3DAmerica%2FToronto&sslmode=disable",
		"postgres://u@h/db":                              "postgres://u@h/db?options=-c%20TimeZone%3DAmerica%2FToronto",
		"postgres://u@h/db?options=-c%20TimeZone%3DUTC":  "postgres://u@h/db?options=-c%20TimeZone%3DUTC",
		"postgres://u@h/db?timezone=UTC":                 "postgres://u@h/db?timezone=UTC",
		"postgres://u@h/db?options=-c%20work_mem%3D64MB": "postgres://u@h/db?options=-c%20work_mem%3D64MB%20-c%20TimeZone%3DAmerica%2FToronto",
		"host=h dbname=db":                               `host=h dbname=db options='-c TimeZone=America/Toronto'`,
		"host=h timezone=UTC":                            "host=h timezone=UTC",
		"host=h options='-c TimeZone=UTC'":               "host=h options='-c TimeZone=UTC'",
		"host=h options='-c work_mem=64MB'":              "host=h options='-c work_mem=64MB -c TimeZone=America/Toronto'",
	}
	for input, want := range cases {
		if got := DSN(input); got != want {
			t.Errorf("DSN(%q) = %q, want %q", input, got, want)
		}
	}
	t.Setenv(TimeZoneEnv, "Asia/Kolkata")
	if got := DSN("postgres://u@h/db"); got != "postgres://u@h/db?options=-c%20TimeZone%3DAsia%2FKolkata" {
		t.Errorf("override: DSN = %q", got)
	}
}
