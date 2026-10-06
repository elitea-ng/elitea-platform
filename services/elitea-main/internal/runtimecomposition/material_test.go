package runtimecomposition

import (
	"reflect"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
)

// oneDirectoryEnvironment is validEnvironment with every file path moved under
// one directory, which is the layout that deployment mounts.
func oneDirectoryEnvironment() map[string]string {
	values := validEnvironment()
	values["ELITEA_RUNTIME_SIGNING_KEY_FILE"] = "/run/elitea-runtime/command-signing-key.pem"
	values["ELITEA_RUNTIME_VERIFICATION_KEYRING_FILE"] = "/run/elitea-runtime/command-signing-keyring.json"
	for _, prefix := range []string{"CONTROL", "OUTPUT", "CONTENT"} {
		lower := strings.ToLower(prefix)
		values["ELITEA_RUNTIME_"+prefix+"_TLS_CERT_FILE"] = "/run/elitea-runtime/" + lower + "-server.crt"
		values["ELITEA_RUNTIME_"+prefix+"_TLS_KEY_FILE"] = "/run/elitea-runtime/" + lower + "-server.key"
		values["ELITEA_RUNTIME_"+prefix+"_TLS_CLIENT_CA_FILE"] = "/run/elitea-runtime/runtime-ca.crt"
	}
	return values
}

func TestMaterialFilesCoverEveryFilePathTheConfigurationCarries(t *testing.T) {
	config, err := ConfigFromEnv(mapLookup(oneDirectoryEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	files, err := config.MaterialFiles()
	if err != nil {
		t.Fatal(err)
	}
	listed := make(map[string]securefile.Permissions, len(files))
	for _, file := range files {
		if _, duplicate := listed[file.Path]; duplicate {
			t.Fatalf("%s is listed more than once", file.Path)
		}
		listed[file.Path] = file.Permissions
	}

	// Derive the expectation from Config itself. A new file field fails this
	// test until MaterialFiles lists it, so the inventory cannot go stale while
	// the configuration grows.
	for _, path := range configuredFilePaths(t, reflect.ValueOf(config)) {
		if _, found := listed[path]; !found {
			t.Fatalf("the configuration names %s, and MaterialFiles does not list it", path)
		}
	}
	if len(listed) != 9 {
		t.Fatalf("expected 9 distinct material files, got %d: %v", len(listed), listed)
	}
}

// configuredFilePaths collects every non-empty string field whose name ends in
// File or Path, at any depth of the configuration.
func configuredFilePaths(t *testing.T, value reflect.Value) []string {
	t.Helper()
	var paths []string
	structType := value.Type()
	for index := range structType.NumField() {
		field := structType.Field(index)
		// The runtime NATS client identity is a cert-manager Secret mounted on
		// its own, not runtime material (MaterialFiles' comment).
		if field.Name == "NATSMaterial" {
			continue
		}
		switch value.Field(index).Kind() {
		case reflect.Struct:
			paths = append(paths, configuredFilePaths(t, value.Field(index))...)
		case reflect.String:
			if !strings.HasSuffix(field.Name, "File") && !strings.HasSuffix(field.Name, "Path") {
				continue
			}
			if text := value.Field(index).String(); text != "" {
				paths = append(paths, text)
			}
		default:
		}
	}
	return paths
}

func TestMaterialFilesKeepsTheStricterProfileForASharedPath(t *testing.T) {
	config, err := ConfigFromEnv(mapLookup(oneDirectoryEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	files, err := config.MaterialFiles()
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]securefile.Permissions{
		"/run/elitea-runtime/command-signing-key.pem":      securefile.PrivateMaterial,
		"/run/elitea-runtime/command-signing-keyring.json": securefile.PublicMaterial,
		"/run/elitea-runtime/runtime-ca.crt":               securefile.PublicMaterial,
		"/run/elitea-runtime/control-server.crt":           securefile.PublicMaterial,
		"/run/elitea-runtime/control-server.key":           securefile.PrivateMaterial,
		"/run/elitea-runtime/output-server.crt":            securefile.PublicMaterial,
		"/run/elitea-runtime/output-server.key":            securefile.PrivateMaterial,
		"/run/elitea-runtime/content-server.crt":           securefile.PublicMaterial,
		"/run/elitea-runtime/content-server.key":           securefile.PrivateMaterial,
	}
	for _, file := range files {
		want, found := expected[file.Path]
		if !found {
			t.Fatalf("unexpected material file %s", file.Path)
		}
		if file.Permissions != want {
			t.Fatalf("%s carries profile %d, expected %d", file.Path, file.Permissions, want)
		}
	}
	if len(files) != len(expected) {
		t.Fatalf("expected %d material files, got %d", len(expected), len(files))
	}
}

func TestMaterialFilesAreSortedAndRefusedWhileTheRuntimeIsOff(t *testing.T) {
	config, err := ConfigFromEnv(mapLookup(oneDirectoryEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	files, err := config.MaterialFiles()
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index < len(files); index++ {
		if files[index-1].Path >= files[index].Path {
			t.Fatalf("material files are not in a stable order: %s then %s", files[index-1].Path, files[index].Path)
		}
	}

	if _, err := (Config{}).MaterialFiles(); err == nil {
		t.Fatal("a disabled runtime reported material files")
	}
	if _, err := (Config{}).MaterialDirectory(); err == nil {
		t.Fatal("a disabled runtime reported a material directory")
	}
}

func TestMaterialDirectoryRefusesPathsThatOneMountCannotServe(t *testing.T) {
	config, err := ConfigFromEnv(mapLookup(oneDirectoryEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	directory, err := config.MaterialDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if directory != "/run/elitea-runtime" {
		t.Fatalf("material directory = %q", directory)
	}

	// validEnvironment spreads the files over /run/secrets and /run/config. One
	// volume cannot present both, so this must be an error and not a silent
	// choice of one of them.
	spread, err := ConfigFromEnv(mapLookup(validEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spread.MaterialDirectory(); err == nil {
		t.Fatal("material paths in two directories were accepted as one mount")
	}

	rootLevel := oneDirectoryEnvironment()
	for name, value := range rootLevel {
		if strings.HasSuffix(name, "_FILE") {
			rootLevel[name] = "/" + value[len("/run/elitea-runtime/"):]
		}
	}
	atRoot, err := ConfigFromEnv(mapLookup(rootLevel))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := atRoot.MaterialDirectory(); err == nil {
		t.Fatal("the filesystem root was accepted as the material directory")
	}
}
