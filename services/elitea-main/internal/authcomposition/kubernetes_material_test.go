package authcomposition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/materialinstall"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
)

// The mode that the kubelet gives each key of a Secret volume when the pod
// declares defaultMode 0444. The init container must read the keys, and the
// kubelet owns them as root, so the read bit for other users is the only one
// that reaches a nonroot process without fsGroup.
const secretVolumeMode = 0o444

// The password of the one Form user that writeMaterialFixture configures.
const fixtureLogin, fixturePassword = "admin", "correct horse battery staple"

// TestSecureFileRefusesTheAuthenticationSecretVolume states the defect that
// issue #444 is about. Without it the tests below would rest on an assumption.
func TestSecureFileRefusesTheAuthenticationSecretVolume(t *testing.T) {
	pod := newAuthDeployment(t)

	for _, file := range pod.config.materialFiles() {
		path := filepath.Join(pod.source, filepath.Base(file.Path))
		if _, err := securefile.Read(path, 1<<20, file.Permissions); err == nil {
			t.Fatalf("securefile accepted the Secret volume path for the %s file, so this install has no reason to exist", file.Purpose)
		} else if !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("the Secret volume was refused for another reason: %v", err)
		}
	}
}

// TestInstalledAuthenticationMaterialAuthenticates is the positive case. It
// installs from the kubelet's own layout, materializes the plane from the
// copies, and then makes a real authentication decision with the Form users
// file that the copy produced.
//
// A refusal is not a pass. The test requires the correct password to succeed
// AND a wrong password to fail.
func TestInstalledAuthenticationMaterialAuthenticates(t *testing.T) {
	pod := newAuthDeployment(t)
	pod.install(t)

	for _, file := range pod.config.materialFiles() {
		info, err := os.Lstat(file.Path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("%s is not a regular file, so securefile refuses it", file.Path)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s carries mode %o, and the install must leave owner-only bits", file.Path, info.Mode().Perm())
		}
	}

	material, err := materialize(pod.config)
	if err != nil {
		t.Fatalf("the authentication plane cannot materialize from the installed copies: %v", err)
	}
	defer material.destroy()

	assertion, err := material.formProvider.
		AssertionVerifier(browserapp.FormSubmission{Login: fixtureLogin, Password: fixturePassword}).
		Verify(context.Background(), browserflow.VerificationContext{
			Provider:             browserapp.FormProviderName,
			OriginatingSessionID: "origin-session",
		})
	if err != nil {
		t.Fatalf("the installed Form users file did not authenticate the configured user: %v", err)
	}
	if assertion.Provider != browserapp.FormProviderName || assertion.ProviderReference != fixtureLogin {
		t.Fatalf("unexpected assertion: %+v", assertion)
	}

	if _, err := material.formProvider.
		AssertionVerifier(browserapp.FormSubmission{Login: fixtureLogin, Password: "not the password"}).
		Verify(context.Background(), browserflow.VerificationContext{
			Provider:             browserapp.FormProviderName,
			OriginatingSessionID: "origin-session",
		}); err == nil {
		t.Fatal("a wrong password authenticated, so the decision proves nothing")
	}
}

// TestInstallRemovesAuthenticationMaterialTheSecretNoLongerCarries is the
// defect that a local container run found for issue #404. The destination
// survives a container restart, so a file that an earlier run installed would
// still satisfy the read-back check, and the pod would keep serving a key that
// the operator had already removed from the Secret.
func TestInstallRemovesAuthenticationMaterialTheSecretNoLongerCarries(t *testing.T) {
	pod := newAuthDeployment(t)
	pod.install(t)

	removed := pod.config.Credentials.PATSigningKeyFile
	before, err := os.ReadFile(removed)
	if err != nil {
		t.Fatal(err)
	}
	pod.removeSecretKey(t, filepath.Base(removed))

	if _, err := pod.tryInstall(); err == nil {
		t.Fatal("the second run accepted a Secret that no longer carries the PAT signing key")
	} else if !strings.Contains(err.Error(), filepath.Base(removed)) {
		t.Fatalf("the refusal does not name the missing file: %v", err)
	}
	if _, err := os.Lstat(removed); err == nil {
		t.Fatalf("the stale %s survived, so the plane would keep serving the old contents", filepath.Base(removed))
	}
	if _, err := materialize(pod.config); !errors.Is(err, ErrInvalidMaterial) {
		t.Fatalf("the plane materialized without the removed file: %v", err)
	}
	clear(before)
}

// TestMissingAuthenticationMaterialStopsTheComposition is the negative that
// issue #444 asks for. A missing file must stop the plane. It must not fall
// back to a weaker authentication path.
func TestMissingAuthenticationMaterialStopsTheComposition(t *testing.T) {
	for _, file := range newAuthDeployment(t).config.materialFiles() {
		t.Run(file.Purpose, func(t *testing.T) {
			pod := newAuthDeployment(t)
			pod.install(t)
			target := pod.pathFor(t, file.Purpose)
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if _, err := materialize(pod.config); !errors.Is(err, ErrInvalidMaterial) {
				t.Fatalf("the plane materialized without %s: %v", target, err)
			}
		})
	}
}

// TestInstalledAuthenticationMaterialStillRefusesAWorldReadablePrivateFile
// breaks the property on purpose. securefile is unchanged, so it must still
// refuse, and a second install must repair the mode.
func TestInstalledAuthenticationMaterialStillRefusesAWorldReadablePrivateFile(t *testing.T) {
	pod := newAuthDeployment(t)
	pod.install(t)

	private := pod.config.Credentials.AttemptKeyFile
	for _, mode := range []os.FileMode{0o644, 0o604, 0o640, 0o606} {
		if err := os.Chmod(private, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := securefile.Read(private, 1<<20, securefile.PrivateMaterial); err == nil {
			t.Fatalf("securefile accepted private material at mode %o", mode)
		}
		if _, err := materialize(pod.config); !errors.Is(err, ErrInvalidMaterial) {
			t.Fatalf("the plane materialized from private material at mode %o: %v", mode, err)
		}
	}

	pod.install(t)
	info, err := os.Lstat(private)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("a second install left mode %o", info.Mode().Perm())
	}
}

// TestMaterialDirectoryRefusesPathsOneMountCannotServe keeps the chart honest.
// One volume mount serves one directory.
func TestMaterialDirectoryRefusesPathsOneMountCannotServe(t *testing.T) {
	config := writeMaterialFixture(t)
	directory, err := config.MaterialDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if directory != filepath.Dir(config.Credentials.AttemptKeyFile) {
		t.Fatalf("MaterialDirectory = %q", directory)
	}

	config.Credentials.PATSigningKeyFile = "/etc/elitea-elsewhere/pat-key"
	if _, err := config.MaterialDirectory(); err == nil {
		t.Fatal("material paths in two directories were accepted")
	}
}

// authDeployment is one temporary stand-in for the pod: a Secret volume in the
// kubelet's own layout, and the empty directory that both containers mount.
type authDeployment struct {
	config      Config
	source      string
	destination string
	keys        map[string][]byte
}

func newAuthDeployment(t *testing.T) authDeployment {
	t.Helper()
	// writeMaterialFixture writes the three files as real files in one
	// directory. That directory becomes the DESTINATION here, and its contents
	// move into a Secret volume that the install must copy back.
	config := writeMaterialFixture(t)
	destination, err := config.MaterialDirectory()
	if err != nil {
		t.Fatal(err)
	}
	source := destination + "-source"
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}

	keys := make(map[string][]byte, 3)
	for _, file := range config.materialFiles() {
		contents, err := os.ReadFile(file.Path)
		if err != nil {
			t.Fatal(err)
		}
		keys[filepath.Base(file.Path)] = contents
		if err := os.Remove(file.Path); err != nil {
			t.Fatal(err)
		}
	}
	pod := authDeployment{config: config, source: source, destination: destination, keys: keys}
	pod.writeSecretVolume(t)
	return pod
}

// writeSecretVolume reproduces the layout that the kubelet writes for a Secret
// volume mounted whole: one timestamped directory that holds the real files, a
// "..data" symlink to it, and one relative symlink for each key.
func (pod authDeployment) writeSecretVolume(t *testing.T) {
	t.Helper()
	data := filepath.Join(pod.source, projectedDirectory)
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, contents := range pod.keys {
		path := filepath.Join(data, name)
		if err := os.WriteFile(path, contents, secretVolumeMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, secretVolumeMode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(projectedDirectory, filepath.Join(pod.source, "..data")); err != nil {
		t.Fatal(err)
	}
	for name := range pod.keys {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(pod.source, name)); err != nil {
			t.Fatal(err)
		}
	}
}

const projectedDirectory = "..2026_08_15_09_41_07.4184283913"

// removeSecretKey deletes one key from the projected directory and from the
// symlink farm, the way a Secret that misses an entry presents itself.
func (pod authDeployment) removeSecretKey(t *testing.T, name string) {
	t.Helper()
	for _, path := range []string{
		filepath.Join(pod.source, projectedDirectory, name),
		filepath.Join(pod.source, name),
	} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

func (pod authDeployment) install(t *testing.T) {
	t.Helper()
	if _, err := pod.tryInstall(); err != nil {
		t.Fatal(err)
	}
}

func (pod authDeployment) tryInstall() ([]string, error) {
	required, err := pod.config.MaterialFiles()
	if err != nil {
		return nil, err
	}
	files := make([]materialinstall.File, 0, len(required))
	for _, file := range required {
		files = append(files, materialinstall.File{Path: file.Path, Permissions: file.Permissions})
	}
	return materialinstall.Install(pod.source, pod.destination, files)
}

func (pod authDeployment) pathFor(t *testing.T, purpose string) string {
	t.Helper()
	for _, file := range pod.config.materialFiles() {
		if file.Purpose == purpose {
			return file.Path
		}
	}
	t.Fatalf("no material file has purpose %q", purpose)
	return ""
}
