package runtimecomposition

import (
	"errors"
	"path/filepath"
	"sort"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
)

// MaterialFile is one deployment file that the enabled runtime plane opens at
// start, with the permission profile that its reader applies.
type MaterialFile struct {
	Path        string
	Permissions securefile.Permissions
}

// MaterialFiles lists every file this configuration reads, once for each path.
//
// The list comes from the same Config that the composition root builds, so a
// new file cannot appear in one place and stay unknown in the other. Deployment
// tools use it to place the material, and to prove that the material is
// readable before the service starts.
//
// One path can serve more than one reader. The runtime CA is the trust root for
// the Redis client and for all three listeners. Such a path keeps the stricter
// profile, because the stricter profile also satisfies the other one: a file
// with owner-only bits passes the public-material check as well.
func (c Config) MaterialFiles() ([]MaterialFile, error) {
	if !c.Enabled {
		return nil, errors.New("the runtime plane is not enabled, so it reads no material")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	byPath := make(map[string]securefile.Permissions, 12)
	add := func(path string, permissions securefile.Permissions) {
		if existing, found := byPath[path]; found && stricterMaterial(existing, permissions) {
			return
		}
		byPath[path] = permissions
	}
	add(c.RedisPasswordFile, securefile.PrivateMaterial)
	add(c.RedisCAFile, securefile.PublicMaterial)
	add(c.SigningKeyFile, securefile.PrivateMaterial)
	add(c.VerificationKeyringFile, securefile.PublicMaterial)
	if c.CodeOwnerRecovery != nil {
		add(c.CodeOwnerRecovery.CertificateChainPath, securefile.PublicMaterial)
		add(c.CodeOwnerRecovery.PrivateKeyPath, securefile.PrivateMaterial)
		add(c.CodeOwnerRecovery.ServerCAPath, securefile.PublicMaterial)
	}
	if c.CodePlatform != nil {
		add(c.CodePlatform.ContentKeysFile, securefile.PrivateMaterial)
	}
	if c.CodeDebugArtifacts != nil {
		add(c.CodeDebugArtifacts.AgentStateDSNFile, securefile.PrivateMaterial)
	}
	if c.RustCompiledSnapshots != nil {
		add(c.RustCompiledSnapshots.ProfilesFile, securefile.PublicMaterial)
		add(c.RustCompiledSnapshots.AgentStateDSNFile, securefile.PrivateMaterial)
	}
	for _, listener := range []runtimeListenerFiles{
		{c.ControlTLS.CertificateChainPath, c.ControlTLS.PrivateKeyPath, c.ControlTLS.ClientCAPath},
		{c.OutputTLS.CertificateChainPath, c.OutputTLS.PrivateKeyPath, c.OutputTLS.ClientCAPath},
		{c.ContentTLS.CertificateChainPath, c.ContentTLS.PrivateKeyPath, c.ContentTLS.ClientCAPath},
	} {
		add(listener.certificateChain, securefile.PublicMaterial)
		add(listener.privateKey, securefile.PrivateMaterial)
		add(listener.clientCA, securefile.PublicMaterial)
	}
	files := make([]MaterialFile, 0, len(byPath))
	for path, permissions := range byPath {
		if !validPrivateConfigPath(path) {
			return nil, errors.New("runtime material path must be absolute and canonical")
		}
		files = append(files, MaterialFile{Path: path, Permissions: permissions})
	}
	sort.Slice(files, func(first, second int) bool { return files[first].Path < files[second].Path })
	return files, nil
}

type runtimeListenerFiles struct {
	certificateChain string
	privateKey       string
	clientCA         string
}

// MaterialDirectory reports the one directory that holds every material file.
//
// Deployment mounts the material as a directory, so the paths must agree on
// one parent. A configuration that spreads them over more than one directory
// cannot be served by one mount, and this reports that as an error instead of
// letting the pod start and then fail on the file it cannot open.
func (c Config) MaterialDirectory() (string, error) {
	files, err := c.MaterialFiles()
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", errors.New("the runtime plane names no material file")
	}
	directory := filepath.Dir(files[0].Path)
	for _, file := range files {
		if filepath.Dir(file.Path) != directory {
			return "", errors.New("every runtime material file must sit in one directory")
		}
	}
	if directory == "/" || !filepath.IsAbs(directory) {
		return "", errors.New("the runtime material directory must be an absolute path below the root")
	}
	return directory, nil
}

func stricterMaterial(existing, candidate securefile.Permissions) bool {
	return existing == securefile.PrivateMaterial && candidate != securefile.PrivateMaterial
}
