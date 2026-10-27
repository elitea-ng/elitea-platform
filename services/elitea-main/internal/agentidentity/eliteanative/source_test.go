package eliteanative_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/eliteanative"
)

// KubeletSecretVolume lays files out as the kubelet's atomic writer does for
// a Secret volume: <dir>/<key> -> ..data/<key>, ..data -> ..<timestamp>/,
// files with the volume's defaultMode. It answers the mount directory.
func kubeletSecretVolume(t *testing.T, mode os.FileMode, files map[string][]byte) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(dir, "..2026_10_27_12_00_00.123456789")
	if err := os.Mkdir(stamp, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		path := filepath.Join(stamp, name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Base(stamp), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The chart mounts the ring's keys as a whole-directory Secret volume with
// defaultMode 0444; the default reader must load them.
func TestReadMountedKeyAcceptsTheKubeletSecretVolumeLayout(t *testing.T) {
	material, _ := ecPEM(t)
	dir := kubeletSecretVolume(t, 0o444, map[string][]byte{"es.pem": material})
	got, err := eliteanative.ReadMountedKey(filepath.Join(dir, "es.pem"))
	if err != nil || string(got) != string(material) {
		t.Fatalf("ReadMountedKey = %v", err)
	}
	// The whole path through LoadKeyRing with the default reader.
	cfg := baseConfig()
	cfg.Keys = []eliteanative.KeySpec{{KeyID: "es-1", Algorithm: eliteanative.ES256, File: filepath.Join(dir, "es.pem")}}
	cfg.RelyingParties = cfg.RelyingParties[:1]
	if _, err := eliteanative.LoadKeyRing(context.Background(), cfg, eliteanative.KeySources{}, nil); err != nil {
		t.Fatalf("LoadKeyRing over a Secret volume: %v", err)
	}
	// A plain owner-only file (a copy, a subPath mount) is read too.
	plain := filepath.Join(dir, "plain.pem")
	if err := os.WriteFile(plain, material, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := eliteanative.ReadMountedKey(plain); err != nil {
		t.Fatalf("a plain 0400 file: %v", err)
	}
}

func TestReadMountedKeyRefusesWritableEscapingOrMalformedFiles(t *testing.T) {
	material, _ := ecPEM(t)
	for name, mode := range map[string]os.FileMode{"group-writable": 0o464, "world-writable": 0o446, "executable": 0o555} {
		dir := kubeletSecretVolume(t, mode, map[string][]byte{"es.pem": material})
		if _, err := eliteanative.ReadMountedKey(filepath.Join(dir, "es.pem")); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A symlink that leaves the mount directory.
	outside := kubeletSecretVolume(t, 0o444, map[string][]byte{"es.pem": material})
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "es.pem"), filepath.Join(dir, "es.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := eliteanative.ReadMountedKey(filepath.Join(dir, "es.pem")); err == nil {
		t.Error("a symlink out of the mount was accepted")
	}
	// A file in a directory anyone can write.
	open := t.TempDir()
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	openDir, _ := filepath.EvalSymlinks(open)
	if err := os.WriteFile(filepath.Join(openDir, "es.pem"), material, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := eliteanative.ReadMountedKey(filepath.Join(openDir, "es.pem")); err == nil {
		t.Error("a key in a world-writable directory was accepted")
	}
	for _, path := range []string{"es.pem", outside + "/./es.pem", filepath.Join(outside, "missing.pem")} {
		if _, err := eliteanative.ReadMountedKey(path); err == nil {
			t.Errorf("%q: accepted", path)
		}
	}
}
