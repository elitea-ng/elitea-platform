// Command elitea-runtime-material installs the runtime plane's private
// material where internal/security/securefile can read it.
//
// # Why the command exists
//
// The runtime plane reads its keys, passwords and certificates through
// securefile. That reader refuses a path that resolves through a symlink, and
// it requires owner-only bits on private material. A Kubernetes Secret volume
// satisfies neither condition, so the material must be copied.
//
// internal/security/materialinstall holds the copy, and its package comment
// gives the full reason. This command supplies the two facts that the copy
// needs for the runtime plane: which files the service opens, and where they
// must go.
//
// # What it copies, and what it proves
//
// The command copies every key of the Secret volume. It then reads back every
// file that the runtime configuration names, with the same permission profile
// that the service applies, and fails if any read fails. A missing key, a
// truncated file or a wrong mode therefore stops the pod in this container,
// with a message, instead of in a restart loop of the service container.
//
// Usage:
//
//	elitea-runtime-material -source /run/elitea-runtime-source
//
// The destination is not an argument. The command reads the runtime
// environment block, which already names every file that the service opens,
// and it writes into the one directory that holds them. A second knob could
// disagree with that block; a derived destination cannot.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/runtimecomposition"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/security/materialinstall"
)

const (
	exitInstalled = 0
	exitFailed    = 1
	exitUsage     = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("elitea-runtime-material", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("source", "", "absolute path of the directory that holds the runtime material keys")
	if err := flags.Parse(arguments); err != nil {
		return exitUsage
	}
	if *source == "" || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage: elitea-runtime-material -source <directory>")
		return exitUsage
	}

	written, err := install(*source, os.LookupEnv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "elitea-runtime-material: %v\n", err)
		return exitFailed
	}
	for _, name := range written {
		_, _ = fmt.Fprintf(stdout, "installed %s\n", name)
	}
	_, _ = fmt.Fprintf(stdout, "elitea-runtime-material: %d files installed\n", len(written))
	return exitInstalled
}

// install derives the required files and the destination from the runtime
// environment block, and then copies the material.
func install(source string, lookup runtimecomposition.LookupEnv) ([]string, error) {
	config, err := runtimecomposition.ConfigFromEnv(lookup)
	if err != nil {
		return nil, fmt.Errorf("read the runtime environment block: %w", err)
	}
	if !config.Enabled {
		return nil, errors.New("ELITEA_RUNTIME_ENABLED is not true, so there is no material to install")
	}
	required, err := config.MaterialFiles()
	if err != nil {
		return nil, err
	}
	destination, err := config.MaterialDirectory()
	if err != nil {
		return nil, err
	}
	files := make([]materialinstall.File, 0, len(required))
	for _, file := range required {
		files = append(files, materialinstall.File{Path: file.Path, Permissions: file.Permissions})
	}
	return materialinstall.Install(source, destination, files)
}
