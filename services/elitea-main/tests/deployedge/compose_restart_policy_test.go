// This file gates the compose restart policies (FU-275 C5). A long-running
// service without one stays down after it exits: on 2026-11-02, -03 and -05
// Main exited on a database or NATS hiccup and nothing started it again, so
// every run on that stack stopped. A one-shot job (migrate, bootstrap, init)
// with one would loop on its own failure instead of failing the stack's start.
//
// The rule is read from the files themselves, so a new service cannot slip
// through: a service another service waits on with
// `condition: service_completed_successfully` is a one-shot and must say
// `restart: "no"`; every other service must say `restart: unless-stopped`.
// A service declared in several files (a base and its overlays) must carry the
// policy in at least one of them, and never two different ones.
//
// RUN IT WITH -count=1: the compose files live in deploy/, outside this module.
package deployedge_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

type restartComposeService struct {
	Image     string    `yaml:"image"`
	Build     yaml.Node `yaml:"build"`
	Restart   string    `yaml:"restart"`
	DependsOn yaml.Node `yaml:"depends_on"`
}

type restartComposeFile struct {
	Services map[string]restartComposeService `yaml:"services"`
}

// completedDependencies are the services this one waits on to finish.
func (s restartComposeService) completedDependencies() []string {
	var names []string
	if s.DependsOn.Kind != yaml.MappingNode {
		return names
	}
	for index := 0; index+1 < len(s.DependsOn.Content); index += 2 {
		name, spec := s.DependsOn.Content[index].Value, s.DependsOn.Content[index+1]
		for field := 0; field+1 < len(spec.Content); field += 2 {
			if spec.Content[field].Value == "condition" && spec.Content[field+1].Value == "service_completed_successfully" {
				names = append(names, name)
			}
		}
	}
	return names
}

func TestEveryComposeServiceHasTheRestartPolicyItsLifetimeNeeds(t *testing.T) {
	root := repoRoot(t)
	files := map[string]restartComposeFile{}
	oneShots := map[string]string{} // one-shot -> a file that waits on it
	for _, relative := range sortedKeys(composeFilesUnderDeploy(t, root)) {
		raw, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Fatal(err)
		}
		var file restartComposeFile
		if err := yaml.Unmarshal(raw, &file); err != nil {
			t.Fatalf("%s: %v", relative, err)
		}
		files[relative] = file
		for _, service := range file.Services {
			for _, dependency := range service.completedDependencies() {
				oneShots[dependency] = relative
			}
		}
	}

	// A base file brings up a stack on its own (it declares the database); an
	// overlay is layered on a base and may inherit a policy from it for a
	// service it re-declares.
	isBase := func(file restartComposeFile) bool { return file.Services["postgres"].Image != "" }
	inheritable := map[string]bool{}
	for _, file := range files {
		if !isBase(file) {
			continue
		}
		for name, service := range file.Services {
			if service.Restart != "" {
				inheritable[name] = true
			}
		}
	}

	checked := map[string]bool{}
	for _, relative := range sortedKeys(files) {
		file := files[relative]
		names := make([]string, 0, len(file.Services))
		for name := range file.Services {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			service := file.Services[name]
			if service.Image == "" && service.Build.IsZero() {
				continue // an override of a service declared elsewhere
			}
			checked[name] = true
			want, lifetime := "unless-stopped", "a long-running service"
			if waiter, ok := oneShots[name]; ok {
				want, lifetime = "no", "a one-shot job ("+waiter+" waits for it to complete)"
			}
			switch {
			case service.Restart == want:
			case service.Restart == "" && !isBase(file) && inheritable[name]:
			case service.Restart == "":
				t.Errorf("%s: %s has no restart policy; as %s it needs restart: %q", relative, name, lifetime, want)
			default:
				t.Errorf("%s: %s has restart: %q; as %s it needs %q", relative, name, service.Restart, lifetime, want)
			}
		}
	}
	for _, floor := range []string{"elitea-main", "elitea-web", "elitea-worker", "elitea-sandbox", "postgres", "pgbouncer", "nats", "elitea-migrate", "nats-bootstrap"} {
		if !checked[floor] {
			t.Errorf("%s was not found in deploy/, so this gate read an incomplete tree", floor)
		}
	}
	if len(checked) < 30 {
		t.Errorf("checked %d services, fewer than the standalone stack alone declares", len(checked))
	}
}
