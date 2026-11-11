// Command faultsgen writes codes_gen.go and docs/errors/README.md from the
// registry. Run it with `go generate ./libs/go/faults` from the repository
// root (or `go generate .` in libs/go/faults).
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "faultsgen:", err)
		os.Exit(1)
	}
}

func run() error {
	if err := os.WriteFile("codes_gen.go", []byte(faults.GeneratedCodesSource()), 0o644); err != nil {
		return err
	}
	guide := filepath.Join("..", "..", "..", "docs", "errors", "README.md")
	if err := os.MkdirAll(filepath.Dir(guide), 0o755); err != nil {
		return err
	}
	return os.WriteFile(guide, []byte(faults.RenderGuide()), 0o644)
}
