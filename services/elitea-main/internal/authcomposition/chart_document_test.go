package authcomposition

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// chartValuesFile is the values file that ships the authentication
// configuration as a chart value (issue #444).
const chartValuesFile = "deploy/helm/elitea/values-standalone.yaml"

// chartValues is the part of the values file that this test reads. The rest of
// the file is the chart's business.
// The platform ships as ONE chart, so elitea-main's values live under the
// `main` key rather than at the values root.
type chartValues struct {
	Main struct {
		FileConfig struct {
			AuthConfig struct {
				Document yaml.Node `yaml:"document"`
				Material struct {
					MountPath string `yaml:"mountPath"`
				} `yaml:"material"`
			} `yaml:"authConfig"`
		} `yaml:"fileConfig"`
	} `yaml:"main"`
}

// TestShippedChartDocumentIsAValidAuthenticationConfiguration reads the
// authentication document out of the chart and parses it with the loader that
// cmd/elitea-main uses.
//
// The chart renders that document into a ConfigMap. Nothing else in the build
// parses it, so a document that this package refuses would render cleanly, pass
// helm lint, pass kubeconform, and then stop the pod at boot.
//
// It also proves that the three material paths agree with the directory that
// the chart mounts. The chart states the directory, the document states the
// paths, and cmd/elitea-auth-material compares them at pod start. This makes
// the same comparison in the build.
func TestShippedChartDocumentIsAValidAuthenticationConfiguration(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, filepath.FromSlash(chartValuesFile))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the chart values file: %v", err)
	}

	var values chartValues
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("decode %s: %v", chartValuesFile, err)
	}
	authConfig := values.Main.FileConfig.AuthConfig
	if authConfig.Document.IsZero() {
		t.Fatalf("%s carries no fileConfig.authConfig.document, so this test would prove nothing", chartValuesFile)
	}

	document, err := yaml.Marshal(&authConfig.Document)
	if err != nil {
		t.Fatal(err)
	}
	config, err := Parse(document)
	if err != nil {
		t.Fatalf("%s ships an authentication document that cmd/elitea-main refuses: %v", chartValuesFile, err)
	}

	directory, err := config.MaterialDirectory()
	if err != nil {
		t.Fatalf("the shipped document spreads its material over more than one directory: %v", err)
	}
	if authConfig.Material.MountPath == "" {
		t.Fatalf("%s sets no fileConfig.authConfig.material.mountPath", chartValuesFile)
	}
	if directory != filepath.Clean(authConfig.Material.MountPath) {
		t.Fatalf(
			"the shipped document reads its material from %s, and the chart mounts %s. cmd/elitea-auth-material refuses that pair at pod start",
			directory, authConfig.Material.MountPath,
		)
	}

	// Every file has to arrive as a Kubernetes Secret key, and a key is a
	// bounded name. A name that no Secret can carry leaves the pod without that
	// file.
	names := make(map[string]struct{}, len(config.materialFiles()))
	for _, file := range config.materialFiles() {
		name := filepath.Base(file.Path)
		if !validSecretKey(name) {
			t.Fatalf("the %s file is %q, which cannot be a Kubernetes Secret key", file.Purpose, name)
		}
		if _, duplicate := names[name]; duplicate {
			t.Fatalf("two material files are both named %q, and one Secret key cannot serve two purposes", name)
		}
		names[name] = struct{}{}
	}
}

func validSecretKey(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 253 {
		return false
	}
	if len(name) > 1 && name[0] == '.' && name[1] == '.' {
		return false
	}
	for index := range len(name) {
		character := name[index]
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '-', character == '_', character == '.':
		default:
			return false
		}
	}
	return true
}
