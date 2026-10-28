// Command skip-check holds the governed conformance run to the declared-skip
// ledger (scripts/go/declared-skips.txt, #423), the way ci-go holds unit
// packages to it.
//
//	go test -v ... | tee run.log
//	skip-check -declared scripts/go/declared-skips.txt -package <import path> run.log...
//
// It reads `go test -v` output and fails when:
//
//   - a test SKIPPED and no line of the ledger declares it for the package
//     (the silent-skip defect the ledger exists for);
//   - a test the ledger names exactly PASSED: the gate it names has lifted,
//     and the declaration is now a stale excuse waiting to hide a real skip;
//   - the logs hold no test result at all: "nothing ran" and "everything
//     passed" must never read the same.
//
// A declaration that matched nothing in this run is only reported: each
// phase runs a subset of the suite with -run.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

type declaration struct {
	pkg, test, reason string
}

func (d declaration) covers(pkg, test string) bool {
	if d.pkg != pkg {
		return false
	}
	switch {
	case d.test == "*":
		return true
	case strings.HasSuffix(d.test, "/*"):
		prefix := strings.TrimSuffix(d.test, "/*")
		return test == prefix || strings.HasPrefix(test, prefix+"/")
	default:
		return d.test == test
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("skip-check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	declaredPath := flags.String("declared", "", "the declared-skip ledger")
	pkg := flags.String("package", "", "the import path the run's tests belong to")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *declaredPath == "" || *pkg == "" || flags.NArg() == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: skip-check -declared FILE -package PATH LOG...")
		return 2
	}
	declarations, err := load(*declaredPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "skip-check:", err)
		return 2
	}
	results := map[string]string{}
	for _, path := range flags.Args() {
		file, err := os.Open(path)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "skip-check:", err)
			return 2
		}
		err = parse(file, results)
		_ = file.Close()
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "skip-check:", err)
			return 2
		}
	}
	return judge(*pkg, declarations, results, stdout)
}

func load(path string) ([]declaration, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var out []declaration
	scanner := bufio.NewScanner(file)
	for number := 1; scanner.Scan(); number++ {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		var fields []string
		for _, field := range strings.Split(line, "\t") {
			if field = strings.TrimSpace(field); field != "" {
				fields = append(fields, field)
			}
		}
		if len(fields) < 3 {
			return nil, fmt.Errorf("%s:%d: expected <package><TAB><test><TAB><reason>", path, number)
		}
		out = append(out, declaration{pkg: fields[0], test: fields[1], reason: strings.Join(fields[2:], "\t")})
	}
	return out, scanner.Err()
}

var resultLine = regexp.MustCompile(`^\s*--- (PASS|FAIL|SKIP): (\S+) \(`)

// parse records the last verdict of each test name in a `go test -v` log.
func parse(r io.Reader, results map[string]string) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		if match := resultLine.FindStringSubmatch(scanner.Text()); match != nil {
			results[match[2]] = match[1]
		}
	}
	return scanner.Err()
}

func judge(pkg string, declarations []declaration, results map[string]string, out io.Writer) int {
	if len(results) == 0 {
		_, _ = fmt.Fprintln(out, "skip-check: the logs hold no test result; refusing to call an empty run green")
		return 1
	}
	failed := false
	counts := map[string]int{}
	used := map[int]bool{}
	for test, verdict := range results {
		counts[verdict]++
		matched := -1
		for i, d := range declarations {
			if d.covers(pkg, test) {
				matched = i
				break
			}
		}
		switch {
		case verdict == "SKIP" && matched < 0:
			_, _ = fmt.Fprintf(out, "UNDECLARED SKIP %s: add it to the ledger with the gate that blocks it, or make it run\n", test)
			failed = true
		case verdict == "SKIP":
			used[matched] = true
			_, _ = fmt.Fprintf(out, "declared skip  %s: %s\n", test, declarations[matched].reason)
		case verdict == "PASS" && matched >= 0 && declarations[matched].test == test:
			_, _ = fmt.Fprintf(out, "STALE DECLARATION %s: it ran and passed; delete its ledger line\n", test)
			failed = true
		}
	}
	for i, d := range declarations {
		if d.pkg == pkg && !used[i] {
			_, _ = fmt.Fprintf(out, "not exercised  %s (another phase, or filtered by -run)\n", d.test)
		}
	}
	_, _ = fmt.Fprintf(out, "skip-check: %d passed, %d failed, %d skipped\n", counts["PASS"], counts["FAIL"], counts["SKIP"])
	if failed {
		return 1
	}
	return 0
}
