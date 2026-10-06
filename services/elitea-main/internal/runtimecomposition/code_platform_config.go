package runtimecomposition

import (
	"path/filepath"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
)

// CodePlatformConfig is explicit deployment composition, never authored Code YAML.
// Disabled composition opens no additional material and creates no broker consumer.
type CodePlatformConfig struct {
	Enabled         bool
	ContentKeysFile string
}

func (c CodePlatformConfig) MaterialFiles() ([]MaterialFile, error) {
	if !c.Enabled {
		return nil, nil
	}
	if c.ContentKeysFile == "" || !filepath.IsAbs(c.ContentKeysFile) || filepath.Clean(c.ContentKeysFile) != c.ContentKeysFile {
		return nil, storage.ErrInvalidKey
	}
	return []MaterialFile{{Path: c.ContentKeysFile, Permissions: securefile.PrivateMaterial}}, nil
}
func (c CodePlatformConfig) LoadContentKeys() (*storage.CodePlatformContentKeyring, error) {
	if !c.Enabled {
		return nil, nil
	}
	files, err := c.MaterialFiles()
	if err != nil {
		return nil, err
	}
	raw, err := securefile.Read(files[0].Path, storage.CodePlatformContentKeyFileLimit, securefile.PrivateMaterial)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	return storage.ParseCodePlatformContentKeys(raw)
}
