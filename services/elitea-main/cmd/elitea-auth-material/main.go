// Command elitea-auth-material installs the authentication plane's private
// material where internal/security/securefile can read it.
//
// # Why the command exists
//
// internal/authcomposition/material.go opens three files through securefile:
// the browser-attempt key, the PAT signing key and the Form users JSON. securefile refuses a path that resolves
// through a symlink, and it requires owner-only bits on private material. A
// Kubernetes Secret volume gives neither. internal/security/materialinstall
// holds the copy that answers this, and its package comment gives the full
// reason.
//
// Issue #404 solved the same problem for the runtime plane, and
// cmd/elitea-runtime-material is its command. This command is the same
// mechanism for the authentication plane. Both call one copy engine.
//
// # Where the three paths come from
//
// They come from the operator's authentication-configuration document, not
// from a chart value. So the Helm chart cannot know them: the document lives in
// a ConfigMap, and Helm cannot read a ConfigMap while it renders.
//
// This command closes that gap. It reads the SAME document that the service
// reads, through the same loader, and it derives:
//
//   - the three paths, from Config.MaterialFiles;
//   - the destination directory, from Config.MaterialDirectory.
//
// The chart states only the directory that it mounts, and it gives that
// directory to this command as -mount. The command compares the two and
// refuses a disagreement by name. So a values file and an authentication
// document that name different directories stop the pod HERE, with a message,
// instead of leaving the service to fail on a file it cannot open.
//
// Usage:
//
//	elitea-auth-material \
//	  -config /etc/elitea/auth-config.json \
//	  -source /run/elitea-auth-source \
//	  -mount  /run/elitea-auth
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/authcomposition"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/materialinstall"
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
	flags := flag.NewFlagSet("elitea-auth-material", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configuration := flags.String("config", "", "absolute path of the authentication configuration file that the service reads")
	source := flags.String("source", "", "absolute path of the directory that holds the authentication material keys")
	mount := flags.String("mount", "", "absolute path of the directory that the pod mounts for the installed material")
	if err := flags.Parse(arguments); err != nil {
		return exitUsage
	}
	if *configuration == "" || *source == "" || *mount == "" || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage: elitea-auth-material -config <file> -source <directory> -mount <directory>")
		return exitUsage
	}

	written, err := install(*configuration, *source, *mount)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "elitea-auth-material: %v\n", err)
		return exitFailed
	}
	for _, name := range written {
		_, _ = fmt.Fprintf(stdout, "installed %s\n", name)
	}
	_, _ = fmt.Fprintf(stdout, "elitea-auth-material: %d files installed\n", len(written))
	return exitInstalled
}

// install derives the required files and the destination from the
// authentication configuration, and then copies the material.
func install(configuration, source, mount string) ([]string, error) {
	config, err := authcomposition.Load(configuration)
	if err != nil {
		return nil, fmt.Errorf("read the authentication configuration %s: %w", configuration, err)
	}
	required, err := config.MaterialFiles()
	if err != nil {
		return nil, err
	}
	destination, err := config.MaterialDirectory()
	if err != nil {
		return nil, err
	}
	// The chart mounts one directory. The authentication document names the
	// three paths. Neither one can see the other, so compare them here.
	if destination != filepath.Clean(mount) {
		return nil, fmt.Errorf(
			"the authentication configuration %s keeps its material in %s, and the pod mounts %s. Set the chart value that mounts the material to %s, or move the three file paths in the authentication configuration into %s",
			configuration, destination, mount, destination, filepath.Clean(mount),
		)
	}
	files := make([]materialinstall.File, 0, len(required))
	for _, file := range required {
		files = append(files, materialinstall.File{Path: file.Path, Permissions: file.Permissions})
	}
	return materialinstall.Install(source, destination, files)
}
