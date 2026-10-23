package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/security/securefile"
)

// The mode that the kubelet gives each key of a Secret volume when the pod
// declares defaultMode 0444. The material must be readable by the pod's own
// user, and the kubelet owns these files as root, so the read bit for other
// users is the only one that reaches a nonroot process without fsGroup.
const secretVolumeMode = 0o444

// TestSecureFileRefusesAKubernetesSecretVolume states the defect that this
// command answers. Without it the test would be an assumption.
func TestSecureFileRefusesAKubernetesSecretVolume(t *testing.T) {
	fixture := newMaterialFixture(t)
	pod := newDeployment(t, fixture, secretVolumeMode)

	privateKeyPath := filepath.Join(pod.source, signingKeyName)
	if _, err := securefile.Read(privateKeyPath, 1<<20, securefile.PrivateMaterial); err == nil {
		t.Fatal("securefile accepted the Secret volume symlink, so this command has no reason to exist")
	} else if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("the Secret volume was refused for another reason: %v", err)
	}

	// The public profile refuses it too, so no part of the material escapes the
	// problem by being a certificate.
	if _, err := securefile.Read(filepath.Join(pod.source, caName), 1<<20, securefile.PublicMaterial); err == nil {
		t.Fatal("securefile accepted the Secret volume symlink for public material")
	}
}

func TestInstallMakesASecretVolumeReadableByTheRuntime(t *testing.T) {
	fixture := newMaterialFixture(t)
	pod := newDeployment(t, fixture, secretVolumeMode)

	written, err := install(pod.source, pod.lookup)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != len(fixture.keys) {
		t.Fatalf("installed %d files, and the Secret carries %d keys", len(written), len(fixture.keys))
	}

	for name, contents := range fixture.keys {
		path := pod.path(name)
		info := mustStat(t, path)
		if !info.Mode().IsRegular() {
			t.Fatalf("%s is not a regular file, so securefile refuses it", name)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s carries mode %o, and private material must be owner-only", name, info.Mode().Perm())
		}
		installed, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(installed, contents) {
			t.Fatalf("%s does not hold the Secret contents", name)
		}
	}

	// install already verifies through securefile. Read the private key again
	// here, so the assertion is visible in this test and not only in the
	// command.
	privateKey, err := securefile.Read(pod.path(signingKeyName), 1<<20, securefile.PrivateMaterial)
	if err != nil {
		t.Fatalf("the runtime cannot read the installed signing key: %v", err)
	}
	clear(privateKey)
}

// TestInstalledMaterialStillRefusesAWorldReadablePrivateFile breaks the
// property on purpose. securefile is unchanged, so it must still refuse.
func TestInstalledMaterialStillRefusesAWorldReadablePrivateFile(t *testing.T) {
	fixture := newMaterialFixture(t)
	pod := newDeployment(t, fixture, secretVolumeMode)
	if _, err := install(pod.source, pod.lookup); err != nil {
		t.Fatal(err)
	}

	privateKeyPath := pod.path(signingKeyName)
	for _, mode := range []os.FileMode{0o644, 0o604, 0o640, 0o606} {
		if err := os.Chmod(privateKeyPath, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := securefile.Read(privateKeyPath, 1<<20, securefile.PrivateMaterial); err == nil {
			t.Fatalf("securefile accepted private material at mode %o", mode)
		}
	}

	// A second run of the init container repairs the mode, because it replaces
	// the file rather than trusting what is already there.
	if _, err := install(pod.source, pod.lookup); err != nil {
		t.Fatal(err)
	}
	if mode := mustStat(t, privateKeyPath).Mode().Perm(); mode != 0o600 {
		t.Fatalf("a rerun left mode %o", mode)
	}
}

// TestInstallRefusesASymlinkThatLeavesTheSource is the other half of the
// property. A key that resolves outside the mount is refused, and the message
// says so.
func TestInstallRefusesASymlinkThatLeavesTheSource(t *testing.T) {
	fixture := newMaterialFixture(t)
	pod := newDeployment(t, fixture, secretVolumeMode)

	outside := filepath.Join(filepath.Dir(pod.source), "outside-the-mount")
	if err := os.WriteFile(outside, []byte("material from elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(pod.source, "escape.pem")); err != nil {
		t.Fatal(err)
	}

	_, err := install(pod.source, pod.lookup)
	if err == nil {
		t.Fatal("a key that resolves outside the source directory was installed")
	}
	if !strings.Contains(err.Error(), "outside the source directory") {
		t.Fatalf("the refusal does not name the cause: %v", err)
	}
	if _, statErr := os.Lstat(pod.path("escape.pem")); statErr == nil {
		t.Fatal("the escaping key reached the destination")
	}
}

func TestInstallRefusesAnIncompleteSecret(t *testing.T) {
	fixture := newMaterialFixture(t)
	pod := newDeployment(t, fixture, secretVolumeMode)
	removeSecretKey(t, pod, "control-server.key")

	_, err := install(pod.source, pod.lookup)
	if err == nil {
		t.Fatal("an incomplete Secret was accepted")
	}
	if !strings.Contains(err.Error(), "control-server.key") {
		t.Fatalf("the refusal does not name the missing file: %v", err)
	}
}

// TestInstallRemovesMaterialTheSecretNoLongerCarries is the case a local run
// with podman found. The destination survives a container restart, so a file
// that an earlier run installed would still satisfy the check, and the pod
// would serve a key that the operator had already removed from the Secret.
func TestInstallRemovesMaterialTheSecretNoLongerCarries(t *testing.T) {
	fixture := newMaterialFixture(t)
	pod := newDeployment(t, fixture, secretVolumeMode)
	if _, err := install(pod.source, pod.lookup); err != nil {
		t.Fatal(err)
	}

	removeSecretKey(t, pod, "control-server.key")
	_, err := install(pod.source, pod.lookup)
	if err == nil {
		t.Fatal("the second run accepted a Secret that no longer carries control-server.key")
	}
	if !strings.Contains(err.Error(), "control-server.key") {
		t.Fatalf("the refusal does not name the missing file: %v", err)
	}
	if _, statErr := os.Lstat(pod.path("control-server.key")); statErr == nil {
		t.Fatal("the stale key survived in the destination")
	}

	// A file that the Secret never carried goes the same way.
	pod = newDeployment(t, fixture, secretVolumeMode)
	stale := pod.path("left-behind.pem")
	if err := os.WriteFile(stale, []byte("stale material\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := install(pod.source, pod.lookup); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Lstat(stale); statErr == nil {
		t.Fatal("a file the Secret does not carry survived in the destination")
	}
}

// removeSecretKey deletes one key from the projected directory and from the
// symlink farm, the way a Secret that misses an entry presents itself.
func removeSecretKey(t *testing.T, pod deployment, name string) {
	t.Helper()
	for _, path := range []string{
		filepath.Join(pod.source, "..2026_08_15_09_41_07.4184283913", name),
		filepath.Join(pod.source, name),
	} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInstallRefusesAnEnvironmentThatOneMountCannotServe(t *testing.T) {
	fixture := newMaterialFixture(t)
	pod := newDeployment(t, fixture, secretVolumeMode)

	pod.environment["ELITEA_RUNTIME_SIGNING_KEY_FILE"] = "/etc/elitea-elsewhere/" + signingKeyName
	if _, err := install(pod.source, pod.lookup); err == nil {
		t.Fatal("material paths in two directories were accepted")
	}

	off := newDeployment(t, fixture, secretVolumeMode)
	off.environment["ELITEA_RUNTIME_ENABLED"] = "false"
	if _, err := install(off.source, off.lookup); err == nil {
		t.Fatal("the command installed material while the runtime plane is off")
	}

	// One directory for both ends would make the command copy the Secret over
	// itself, and the prune step would then judge the source by its own output.
	same := newDeployment(t, fixture, secretVolumeMode)
	if _, err := install(same.destination, same.lookup); err == nil {
		t.Fatal("the source and the destination were accepted as one directory")
	}
}

// TestInstallSkipsTheKubeletProjectionAndKeepsNoSymlink proves that the
// destination holds real files only. A copied symlink would put the pod back
// where it started.
func TestInstallSkipsTheKubeletProjectionAndKeepsNoSymlink(t *testing.T) {
	fixture := newMaterialFixture(t)
	pod := newDeployment(t, fixture, secretVolumeMode)
	if _, err := install(pod.source, pod.lookup); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(pod.destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(fixture.keys) {
		t.Fatalf("the destination holds %d entries, and the Secret carries %d keys", len(entries), len(fixture.keys))
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "..") {
			t.Fatalf("the kubelet projection entry %s was copied", entry.Name())
		}
		if mustStat(t, filepath.Join(pod.destination, entry.Name())).Mode()&os.ModeSymlink != 0 {
			t.Fatalf("%s is a symlink in the destination", entry.Name())
		}
	}
}

func TestRunReportsUsageWithoutASource(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != exitUsage {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(stderr.String(), "-source") {
		t.Fatalf("the usage message does not name the flag: %q", stderr.String())
	}
}
