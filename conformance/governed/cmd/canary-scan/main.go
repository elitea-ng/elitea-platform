// Command canary-scan fails when any seeded secret canary appears in the
// evidence of a governed conformance run (issue #1124, item 3).
//
//	canary-scan -canaries canaries.json -dir EVIDENCE [-require a,b,...]
//	canary-scan -selftest
//
// canaries.json is what the suite wrote: [{"label": ..., "value": ...}].
// EVIDENCE is what deploy/scripts/governed-conformance.sh collected: the NATS
// tap, every database dumped with pg_dump (input bundles, security contexts,
// agent checkpoints, the audit outbox and events), the container logs (which
// carry the OTLP collector's debug export) and the mocks' journal.
//
// Exit 0 means every required file was read and no canary was found. Exit 1
// is a leak (each hit names the canary's label, never its value). Exit 2 is
// a scan that could not be trusted: no canaries, a required file missing or
// empty, or nothing read at all. "Found nothing" is never assumed from
// "looked at nothing".
//
// -selftest plants a fresh canary in a scratch directory, in the encodings a
// real leak is written in, and exits 0 only when the scan FAILS on it: CI
// runs it before the real scan, so a blind scanner cannot report clean.
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/elitea-ng/elitea-platform/conformance/governed/canary"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("canary-scan", flag.ContinueOnError)
	canaryFile := flags.String("canaries", "", "the suite's canary list (JSON)")
	dir := flags.String("dir", "", "the evidence directory")
	require := flags.String("require", "", "comma-separated files (relative to -dir) that must exist and be non-empty")
	selftest := flags.Bool("selftest", false, "plant a canary and require the scan to fail")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *selftest {
		return selfTest()
	}
	if *canaryFile == "" || *dir == "" {
		fmt.Fprintln(os.Stderr, "canary-scan: -canaries and -dir are required")
		return 2
	}
	raw, err := os.ReadFile(*canaryFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "canary-scan: read the canary list:", err)
		return 2
	}
	var canaries []canary.Canary
	if err := json.Unmarshal(raw, &canaries); err != nil {
		fmt.Fprintln(os.Stderr, "canary-scan: the canary list is not JSON:", err)
		return 2
	}
	for _, name := range strings.Split(*require, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		info, err := os.Stat(filepath.Join(*dir, name))
		if err != nil || info.Size() == 0 {
			fmt.Fprintf(os.Stderr, "canary-scan: required evidence %s is missing or empty\n", name)
			return 2
		}
	}
	return scan(canaries, *dir, *canaryFile)
}

func scan(canaries []canary.Canary, dir, canaryFile string) int {
	scanner, err := canary.NewScanner(canaries)
	if err != nil {
		fmt.Fprintln(os.Stderr, "canary-scan:", err)
		return 2
	}
	absList, _ := filepath.Abs(canaryFile)
	report, err := scanner.ScanDir(dir, func(rel string) bool {
		abs, _ := filepath.Abs(filepath.Join(dir, rel))
		return abs == absList
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "canary-scan:", err)
		return 2
	}
	if report.Files == 0 || report.Bytes == 0 {
		fmt.Fprintln(os.Stderr, "canary-scan: no evidence was read; refusing to report clean")
		return 2
	}
	fmt.Printf("canary-scan: %d canaries, %d files, %d bytes\n", len(canaries), report.Files, report.Bytes)
	for _, file := range report.Read {
		fmt.Println("  read", file)
	}
	if len(report.Hits) > 0 {
		for _, hit := range report.Hits {
			fmt.Println("LEAK", hit.String())
		}
		fmt.Printf("canary-scan: %d hits — a seeded secret reached the evidence\n", len(report.Hits))
		return 1
	}
	fmt.Println("canary-scan: zero hits")
	return 0
}

func selfTest() int {
	dir, err := os.MkdirTemp("", "canary-selftest-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "canary-scan -selftest:", err)
		return 2
	}
	defer func() { _ = os.RemoveAll(dir) }()
	planted, err := canary.New("selftest")
	if err != nil {
		fmt.Fprintln(os.Stderr, "canary-scan -selftest:", err)
		return 2
	}
	files := map[string]string{
		"logs.txt":           "INFO worker token=" + planted.Value + "\n",
		"audit-outbox.jsonl": `{"data":{"blob":"` + base64.StdEncoding.EncodeToString([]byte("x"+planted.Value)) + `"}}` + "\n",
		"clean.txt":          "nothing to see\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "canary-scan -selftest:", err)
			return 2
		}
	}
	list := filepath.Join(dir, "canaries.json")
	raw, _ := json.Marshal([]canary.Canary{planted})
	if err := os.WriteFile(list, raw, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "canary-scan -selftest:", err)
		return 2
	}
	if code := scan([]canary.Canary{planted}, dir, list); code != 1 {
		fmt.Fprintf(os.Stderr, "canary-scan -selftest: a planted leak scanned as exit %d, want 1; the scanner is blind\n", code)
		return 1
	}
	fmt.Println("canary-scan -selftest: the planted leak failed the scan, as it must")
	return 0
}
