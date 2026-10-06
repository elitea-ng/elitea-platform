package authcomposition

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"unicode/utf8"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
)

const (
	minAttemptKeyBytes    = 32
	maxAttemptKeyBytes    = 64
	maxPATSigningKeyBytes = int64(64 << 10)
)

// The purpose names. Each one appears in the load failure message for its file,
// so an operator reads the purpose when a file is wrong.
const (
	purposeAttemptKey    = "browser-attempt key"
	purposePATSigningKey = "PAT signing key"
	purposeFormUsers     = "Form users JSON"
)

var ErrInvalidMaterial = errors.New("invalid authentication composition material")

// MaterialFile is one deployment file that the authentication plane opens at
// start, with the bound and the permission profile that its reader applies.
type MaterialFile struct {
	Purpose     string
	Path        string
	MaxBytes    int64
	Permissions securefile.Permissions
}

// MaterialFiles lists every file this configuration reads, in a stable order.
//
// materialize below reads this same list. So a new file cannot appear in one
// place and stay unknown in the other. Deployment tools use the list to place
// the material, and to prove that the material is readable before the service
// starts.
//
// The paths come from the operator's authentication-configuration document.
// The Helm chart cannot read that document while it renders, so the chart
// states only the directory, and cmd/elitea-auth-material compares the two.
func (config Config) MaterialFiles() ([]MaterialFile, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return config.materialFiles(), nil
}

// materialFiles is the list itself. Validate refuses a configuration whose
// paths are empty, relative or repeated, so every caller inside this package
// gets three distinct absolute paths.
func (config Config) materialFiles() []MaterialFile {
	return []MaterialFile{
		{purposeAttemptKey, config.Credentials.AttemptKeyFile, maxAttemptKeyBytes, securefile.PrivateMaterial},
		{purposePATSigningKey, config.Credentials.PATSigningKeyFile, maxPATSigningKeyBytes, securefile.PrivateMaterial},
		{purposeFormUsers, config.Provider.Form.UsersJSONFile, browserapp.MaxFormConfigurationBytes, securefile.PrivateMaterial},
	}
}

// MaterialDirectory reports the one directory that holds every material file.
//
// Deployment mounts the material as a directory, so the paths must agree on one
// parent. A configuration that spreads them over more than one directory cannot
// be served by one mount. This reports that as an error, instead of letting the
// pod start and then fail on the file it cannot open.
func (config Config) MaterialDirectory() (string, error) {
	files, err := config.MaterialFiles()
	if err != nil {
		return "", err
	}
	directory := filepath.Dir(files[0].Path)
	for _, file := range files {
		if filepath.Dir(file.Path) != directory {
			return "", fmt.Errorf(
				"%w: every authentication material file must sit in one directory, and %s is not in %s",
				ErrInvalidMaterial, file.Path, directory,
			)
		}
	}
	if directory == "/" || !filepath.IsAbs(directory) {
		return "", fmt.Errorf(
			"%w: the authentication material directory must be an absolute path below the root",
			ErrInvalidMaterial,
		)
	}
	return directory, nil
}

// materializedFiles is a short-lived startup value. Call destroy immediately
// after constructors have snapshotted the required material. FormProvider
// retains only keyed password digests; raw Form JSON is never retained here.
type materializedFiles struct {
	attemptKey    []byte
	patSigningKey []byte
	formProvider  *browserapp.FormProvider
}

type materialReference struct {
	purpose  string
	snapshot securefile.Snapshot
}

func materialize(config Config) (*materializedFiles, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("%w: configuration", ErrInvalidMaterial)
	}

	// One list, read in one loop. MaterialFiles is the same list that
	// cmd/elitea-auth-material installs and proves readable, so the deployment
	// tool and the service can never disagree about which files exist.
	required := config.materialFiles()
	references := make([]materialReference, 0, len(required))
	defer func() {
		for index := range references {
			clear(references[index].snapshot.Contents)
		}
	}()

	for _, file := range required {
		snapshot, err := securefile.ReadSnapshot(file.Path, file.MaxBytes, file.Permissions)
		if err != nil {
			// Do not copy os.PathError paths into a potentially shared startup log.
			return nil, fmt.Errorf("%w: load %s file", ErrInvalidMaterial, file.Purpose)
		}
		references = append(references, materialReference{purpose: file.Purpose, snapshot: snapshot})
	}

	if err := validateMaterialReferences(references); err != nil {
		return nil, err
	}

	attemptKeyFile := snapshotFor(references, purposeAttemptKey)
	patKeyFile := snapshotFor(references, purposePATSigningKey)
	formUsersFile := snapshotFor(references, purposeFormUsers)

	if len(attemptKeyFile.Contents) < minAttemptKeyBytes || len(attemptKeyFile.Contents) > maxAttemptKeyBytes {
		return nil, fmt.Errorf("%w: browser-attempt key", ErrInvalidMaterial)
	}
	// Existing Python-issued HS512 PATs require these exact UTF-8 bytes. In
	// particular, do not TrimSpace or consume a terminal newline here.
	if !utf8.Valid(patKeyFile.Contents) || bytes.IndexByte(patKeyFile.Contents, 0) >= 0 {
		return nil, fmt.Errorf("%w: PAT signing key", ErrInvalidMaterial)
	}
	provider, err := browserapp.NewFormProvider(formUsersFile.Contents)
	if err != nil {
		return nil, fmt.Errorf("%w: Form users JSON", ErrInvalidMaterial)
	}

	return &materializedFiles{
		attemptKey:    append([]byte(nil), attemptKeyFile.Contents...),
		patSigningKey: append([]byte(nil), patKeyFile.Contents...),
		formProvider:  provider,
	}, nil
}

// snapshotFor returns the snapshot that the loop above read for one purpose.
// Every purpose in materialFiles is read before this runs, so a miss is not
// reachable; an empty snapshot then fails the content checks that follow.
func snapshotFor(references []materialReference, purpose string) securefile.Snapshot {
	for _, reference := range references {
		if reference.purpose == purpose {
			return reference.snapshot
		}
	}
	return securefile.Snapshot{}
}

func validateMaterialReferences(references []materialReference) error {
	for left := range references {
		for right := left + 1; right < len(references); right++ {
			if references[left].snapshot.SameFile(references[right].snapshot) {
				return fmt.Errorf(
					"%w: %s and %s reference one file",
					ErrInvalidMaterial,
					references[left].purpose,
					references[right].purpose,
				)
			}
			if bytes.Equal(references[left].snapshot.Contents, references[right].snapshot.Contents) {
				return fmt.Errorf(
					"%w: %s and %s reuse one value",
					ErrInvalidMaterial,
					references[left].purpose,
					references[right].purpose,
				)
			}
		}
	}
	return nil
}

func (material *materializedFiles) destroy() {
	if material == nil {
		return
	}
	clear(material.attemptKey)
	clear(material.patSigningKey)
	material.attemptKey = nil
	material.patSigningKey = nil
	material.formProvider = nil
}
