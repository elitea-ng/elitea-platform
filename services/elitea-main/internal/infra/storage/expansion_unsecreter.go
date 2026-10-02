package storage

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
)

const (
	maxCurrentUnsecretDepth      = configurationapp.MaxCurrentExpansionDepth
	maxCurrentUnsecretNodes      = configurationapp.MaxCurrentExpansionNodes
	maxCurrentUnsecretReferences = configurationapp.MaxCurrentExpansionNodes
	maxCurrentUnsecretOutput     = configurationapp.MaxCurrentExpansionStringBytes
	maxCurrentUnsecretNameBytes  = configurationapp.MaxCurrentExpansionIdentifierLength
	maxCurrentUnsecretValueBytes = 256 * 1024
)

var (
	ErrCurrentUnsecretRejected    = errors.New("current secret expansion rejected")
	ErrCurrentUnsecretUnavailable = errors.New("current secret expansion unavailable")
	currentSecretPlaceholder      = regexp.MustCompile(`\{\{secret\.([A-Za-z0-9_]+)\}\}`)
)

// CurrentVaultUnsecreter resolves current {{secret.NAME}} placeholders from
// the configuration owner's vault. It performs exact-name lookups only and
// never enumerates vault contents.
type CurrentVaultUnsecreter struct {
	vaults SecretVaultLoader
}

func NewCurrentVaultUnsecreter(vaults SecretVaultLoader) (*CurrentVaultUnsecreter, error) {
	if vaults == nil {
		return nil, errors.New("current secret vault loader is required")
	}
	return &CurrentVaultUnsecreter{vaults: vaults}, nil
}

// Unsecret returns an owned copy. Project regular values override project
// hidden values through SecretVault.Lookup; project misses fall back only to
// admin regular values. Missing names remain as their original placeholders.
func (u *CurrentVaultUnsecreter) Unsecret(
	ctx context.Context,
	configurationProjectID int32,
	data map[string]any,
) (map[string]any, error) {
	if ctx == nil || configurationProjectID <= 0 {
		return nil, ErrCurrentUnsecretRejected
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if data == nil {
		return nil, nil
	}

	walker := currentUnsecretWalker{
		ctx:       ctx,
		vaults:    u.vaults,
		projectID: int64(configurationProjectID),
		resolved:  make(map[string]currentSecretResolution),
	}
	value, err := walker.clone(data, 0)
	if err != nil {
		return nil, err
	}
	result := value.(map[string]any)
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > maxCurrentUnsecretOutput {
		clearContentBytes(encoded)
		return nil, ErrCurrentUnsecretRejected
	}
	clearContentBytes(encoded)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

type currentSecretResolution struct {
	value string
	found bool
}

type currentUnsecretWalker struct {
	ctx           context.Context
	vaults        SecretVaultLoader
	projectID     int64
	projectVault  SecretVault
	projectLoaded bool
	adminVault    SecretVault
	adminLoaded   bool
	resolved      map[string]currentSecretResolution
	nodes         int
	references    int
	outputBytes   int
}

func (w *currentUnsecretWalker) clone(value any, depth int) (any, error) {
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	if depth > maxCurrentUnsecretDepth {
		return nil, ErrCurrentUnsecretRejected
	}

	switch value := value.(type) {
	case map[string]any:
		if err := w.addContainer(len(value)); err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		cloned := make(map[string]any, len(value))
		for _, key := range keys {
			if err := w.addOutputBytes(len(key)); err != nil {
				return nil, err
			}
			item, err := w.clone(value[key], depth+1)
			if err != nil {
				return nil, err
			}
			cloned[key] = item
		}
		return cloned, nil
	case []any:
		if err := w.addContainer(len(value)); err != nil {
			return nil, err
		}
		cloned := make([]any, len(value))
		for index := range value {
			item, err := w.clone(value[index], depth+1)
			if err != nil {
				return nil, err
			}
			cloned[index] = item
		}
		return cloned, nil
	case string:
		if err := w.addNode(); err != nil {
			return nil, err
		}
		return w.replace(value)
	case json.Number:
		if err := w.addNode(); err != nil {
			return nil, err
		}
		if err := w.addOutputBytes(len(value)); err != nil {
			return nil, err
		}
		return value, nil
	case nil, bool,
		float32, float64,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		if err := w.addNode(); err != nil {
			return nil, err
		}
		return value, nil
	default:
		return nil, ErrCurrentUnsecretRejected
	}
}

type currentStringReplacement struct {
	start int
	end   int
	value string
}

func (w *currentUnsecretWalker) replace(value string) (string, error) {
	replacements := make([]currentStringReplacement, 0)
	cursor := 0
	outputLength := 0
	for cursor < len(value) {
		match := currentSecretPlaceholder.FindStringSubmatchIndex(value[cursor:])
		if match == nil {
			break
		}
		start := cursor + match[0]
		end := cursor + match[1]
		nameStart := cursor + match[2]
		nameEnd := cursor + match[3]
		if err := w.addReference(); err != nil {
			return "", err
		}
		name := value[nameStart:nameEnd]
		if len(name) > maxCurrentUnsecretNameBytes {
			return "", ErrCurrentUnsecretRejected
		}
		resolution, err := w.resolve(name)
		if err != nil {
			return "", err
		}
		replacement := value[start:end]
		if resolution.found {
			replacement = resolution.value
		}
		if !boundedCurrentUnsecretSum(&outputLength, start-cursor) ||
			!boundedCurrentUnsecretSum(&outputLength, len(replacement)) {
			return "", ErrCurrentUnsecretRejected
		}
		replacements = append(replacements, currentStringReplacement{
			start: start,
			end:   end,
			value: replacement,
		})
		cursor = end
	}
	if len(replacements) == 0 {
		if err := w.addOutputBytes(len(value)); err != nil {
			return "", err
		}
		return value, nil
	}
	if !boundedCurrentUnsecretSum(&outputLength, len(value)-cursor) {
		return "", ErrCurrentUnsecretRejected
	}
	if err := w.addOutputBytes(outputLength); err != nil {
		return "", err
	}

	var result strings.Builder
	result.Grow(outputLength)
	cursor = 0
	for _, replacement := range replacements {
		result.WriteString(value[cursor:replacement.start])
		result.WriteString(replacement.value)
		cursor = replacement.end
	}
	result.WriteString(value[cursor:])
	return result.String(), nil
}

func (w *currentUnsecretWalker) resolve(name string) (currentSecretResolution, error) {
	if err := w.ctx.Err(); err != nil {
		return currentSecretResolution{}, err
	}
	if resolution, ok := w.resolved[name]; ok {
		return resolution, nil
	}

	projectVault, err := w.loadProjectVault()
	if err != nil {
		return currentSecretResolution{}, err
	}
	secret, err := projectVault.Lookup(name)
	if err == nil {
		return w.cacheSecret(name, secret)
	}
	if contextErr := w.ctx.Err(); contextErr != nil {
		return currentSecretResolution{}, contextErr
	}
	if !errors.Is(err, centrysecrets.ErrSecretNotFound) {
		return currentSecretResolution{}, currentUnsecretDependencyError(w.ctx, err)
	}

	adminVault, err := w.loadAdminVault()
	if err != nil {
		return currentSecretResolution{}, err
	}
	secret, err = adminVault.LookupRegular(name)
	if err == nil {
		return w.cacheSecret(name, secret)
	}
	if contextErr := w.ctx.Err(); contextErr != nil {
		return currentSecretResolution{}, contextErr
	}
	if !errors.Is(err, centrysecrets.ErrSecretNotFound) {
		return currentSecretResolution{}, currentUnsecretDependencyError(w.ctx, err)
	}
	resolution := currentSecretResolution{}
	w.resolved[name] = resolution
	return resolution, nil
}

func (w *currentUnsecretWalker) loadProjectVault() (SecretVault, error) {
	if w.projectLoaded {
		return w.projectVault, nil
	}
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	vault, err := w.vaults.LoadProjectVault(w.ctx, w.projectID)
	if err != nil || vault == nil {
		return nil, currentUnsecretDependencyError(w.ctx, err)
	}
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	w.projectVault = vault
	w.projectLoaded = true
	return vault, nil
}

func (w *currentUnsecretWalker) cacheSecret(
	name string,
	secret centrysecrets.Secret,
) (currentSecretResolution, error) {
	if err := w.ctx.Err(); err != nil {
		return currentSecretResolution{}, err
	}
	if len(secret.Value) > maxCurrentUnsecretValueBytes {
		return currentSecretResolution{}, ErrCurrentUnsecretRejected
	}
	resolution := currentSecretResolution{value: secret.Value, found: true}
	w.resolved[name] = resolution
	return resolution, nil
}

func (w *currentUnsecretWalker) loadAdminVault() (SecretVault, error) {
	if w.adminLoaded {
		return w.adminVault, nil
	}
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	vault, err := w.vaults.LoadAdminVault(w.ctx)
	if err != nil || vault == nil {
		return nil, currentUnsecretDependencyError(w.ctx, err)
	}
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	w.adminVault = vault
	w.adminLoaded = true
	return vault, nil
}

func (w *currentUnsecretWalker) addContainer(entries int) error {
	if entries < 0 || entries > maxCurrentUnsecretNodes-w.nodes-1 {
		return ErrCurrentUnsecretRejected
	}
	w.nodes++
	return nil
}

func (w *currentUnsecretWalker) addNode() error {
	if w.nodes >= maxCurrentUnsecretNodes {
		return ErrCurrentUnsecretRejected
	}
	w.nodes++
	return nil
}

func (w *currentUnsecretWalker) addReference() error {
	if w.references >= maxCurrentUnsecretReferences {
		return ErrCurrentUnsecretRejected
	}
	w.references++
	return nil
}

func (w *currentUnsecretWalker) addOutputBytes(count int) error {
	if count < 0 || count > maxCurrentUnsecretOutput-w.outputBytes {
		return ErrCurrentUnsecretRejected
	}
	w.outputBytes += count
	return nil
}

func boundedCurrentUnsecretSum(total *int, count int) bool {
	if count < 0 || count > maxCurrentUnsecretOutput-*total {
		return false
	}
	*total += count
	return true
}

func currentUnsecretDependencyError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return ErrCurrentUnsecretUnavailable
}

var _ configurationapp.CurrentExpansionUnsecreter = (*CurrentVaultUnsecreter)(nil)
