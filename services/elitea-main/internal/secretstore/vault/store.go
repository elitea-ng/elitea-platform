package vault

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
)

// valueKey is the one key a secret's KV v2 data map holds.
const valueKey = "value"

var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

const maxPathSegments = 16

// Store is the KV v2 SecretStore. It is safe for concurrent use.
type Store struct {
	client *client
}

var _ secretstore.SecretStore = (*Store)(nil)

// New builds the adapter. It does not dial; the readiness prerequisite does.
func New(cfg Config) (*Store, error) {
	c, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Store{client: c}, nil
}

// apiPath builds /v1/<mount>/<kind>/<path>. The mount must be the configured
// KV mount: a reference cannot steer the adapter onto another engine.
func (s *Store) apiPath(op secretstore.Op, ref secretstore.SecretRef, kind string) (string, error) {
	if err := secretstore.ValidateBackend(op, ref, secretstore.BackendVault); err != nil {
		return "", err
	}
	if ref.Mount != s.client.cfg.KVMount {
		return "", secretstore.Errorf(secretstore.ErrMisconfigured, op, "the reference names another mount")
	}
	segments := strings.Split(ref.Path, "/")
	if ref.Path == "" || len(segments) > maxPathSegments {
		return "", secretstore.Errorf(secretstore.ErrInvalidRequest, op, "malformed secret path")
	}
	for index, segment := range segments {
		if !segmentPattern.MatchString(segment) || strings.Contains(segment, "..") {
			return "", secretstore.Errorf(secretstore.ErrInvalidRequest, op, "malformed secret path")
		}
		segments[index] = url.PathEscape(segment)
	}
	return "/v1/" + s.client.cfg.KVMount + "/" + kind + "/" + strings.Join(segments, "/"), nil
}

type versionMetadata struct {
	CreatedTime  string `json:"created_time"`
	DeletionTime string `json:"deletion_time"`
	Destroyed    bool   `json:"destroyed"`
	Version      int64  `json:"version"`
}

type readResponse struct {
	Data *struct {
		// Data is decoded into RawMessage values so a non-string value is
		// refused rather than coerced.
		Data     map[string]json.RawMessage `json:"data"`
		Metadata *versionMetadata           `json:"metadata"`
	} `json:"data"`
}

// Read returns exactly the requested version. There is no "latest" read.
func (s *Store) Read(ctx context.Context, sc secretstore.StoreContext, ref secretstore.SecretRef, version int64) (secretstore.SecretVersion, error) {
	op := secretstore.OpRead
	if err := sc.Validate(op); err != nil {
		return secretstore.SecretVersion{}, err
	}
	if err := secretstore.ValidateReadVersion(version); err != nil {
		return secretstore.SecretVersion{}, err
	}
	path, err := s.apiPath(op, ref, "data")
	if err != nil {
		return secretstore.SecretVersion{}, err
	}
	status, raw, err := s.client.call(ctx, op, sc.WorkloadScope, http.MethodGet, path+"?version="+strconv.FormatInt(version, 10), nil)
	if err != nil {
		return secretstore.SecretVersion{}, err
	}
	defer clear(raw)
	if status != http.StatusOK {
		return secretstore.SecretVersion{}, mapStatus(op, status, raw)
	}
	var decoded readResponse
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Data == nil || decoded.Data.Metadata == nil {
		return secretstore.SecretVersion{}, secretstore.Errorf(secretstore.ErrUnavailable, op, "malformed read response")
	}
	meta := decoded.Data.Metadata
	if meta.Version != version {
		return secretstore.SecretVersion{}, secretstore.Errorf(secretstore.ErrMisconfigured, op, "backend answered another version")
	}
	if meta.Destroyed || meta.DeletionTime != "" || decoded.Data.Data == nil {
		return secretstore.SecretVersion{}, secretstore.Errorf(secretstore.ErrNotFound, op, "the version is deleted or destroyed")
	}
	encoded, ok := decoded.Data.Data[valueKey]
	if !ok || len(decoded.Data.Data) != 1 {
		return secretstore.SecretVersion{}, secretstore.Errorf(secretstore.ErrMisconfigured, op, "the secret is not in the expected shape")
	}
	var value string
	if err := json.Unmarshal(encoded, &value); err != nil {
		return secretstore.SecretVersion{}, secretstore.Errorf(secretstore.ErrMisconfigured, op, "the secret is not in the expected shape")
	}
	created, _ := time.Parse(time.RFC3339Nano, meta.CreatedTime)
	return secretstore.SecretVersion{
		Version:   version,
		Value:     secretstore.NewRuntimeSecret([]byte(value)),
		CreatedAt: created,
	}, nil
}

type metadataResponse struct {
	Data *struct {
		CASRequired    bool                       `json:"cas_required"`
		CurrentVersion int64                      `json:"current_version"`
		Versions       map[string]versionMetadata `json:"versions"`
		// custom_metadata is deliberately not decoded. It carries no
		// authority (ADR-0011 amendment, KV v2 rule 3).
	} `json:"data"`
}

type mountConfigResponse struct {
	Data *struct {
		CASRequired bool `json:"cas_required"`
	} `json:"data"`
}

// mountCASRequired reads <mount>/config.
func (s *Store) mountCASRequired(ctx context.Context, op secretstore.Op, scope string) (bool, error) {
	status, raw, err := s.client.call(ctx, op, scope, http.MethodGet, "/v1/"+s.client.cfg.KVMount+"/config", nil)
	if err != nil {
		return false, err
	}
	if status != http.StatusOK {
		return false, mapStatus(op, status, raw)
	}
	var decoded mountConfigResponse
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Data == nil {
		return false, secretstore.Errorf(secretstore.ErrUnavailable, op, "malformed mount configuration")
	}
	return decoded.Data.CASRequired, nil
}

// pathMetadata reads <mount>/metadata/<path>. found is false on 404.
func (s *Store) pathMetadata(ctx context.Context, op secretstore.Op, scope, path string) (secretstore.Metadata, bool, error) {
	status, raw, err := s.client.call(ctx, op, scope, http.MethodGet, path, nil)
	if err != nil {
		return secretstore.Metadata{}, false, err
	}
	if status == http.StatusNotFound {
		return secretstore.Metadata{}, false, nil
	}
	if status != http.StatusOK {
		return secretstore.Metadata{}, false, mapStatus(op, status, raw)
	}
	var decoded metadataResponse
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Data == nil {
		return secretstore.Metadata{}, false, secretstore.Errorf(secretstore.ErrUnavailable, op, "malformed metadata response")
	}
	meta := secretstore.Metadata{CurrentVersion: decoded.Data.CurrentVersion, CASRequired: decoded.Data.CASRequired}
	for key, entry := range decoded.Data.Versions {
		number, err := strconv.ParseInt(key, 10, 64)
		if err != nil || number <= 0 {
			return secretstore.Metadata{}, false, secretstore.Errorf(secretstore.ErrUnavailable, op, "malformed metadata response")
		}
		if entry.Destroyed || entry.DeletionTime != "" {
			continue
		}
		meta.Versions = append(meta.Versions, number)
	}
	slices.Sort(meta.Versions)
	return meta, true, nil
}

// Metadata reports the readable versions. CASRequired is true when the mount
// or the path enforces check-and-set.
func (s *Store) Metadata(ctx context.Context, sc secretstore.StoreContext, ref secretstore.SecretRef) (secretstore.Metadata, error) {
	op := secretstore.OpMetadata
	if err := sc.Validate(op); err != nil {
		return secretstore.Metadata{}, err
	}
	path, err := s.apiPath(op, ref, "metadata")
	if err != nil {
		return secretstore.Metadata{}, err
	}
	meta, found, err := s.pathMetadata(ctx, op, sc.WorkloadScope, path)
	if err != nil {
		return secretstore.Metadata{}, err
	}
	if !found {
		return secretstore.Metadata{}, secretstore.Errorf(secretstore.ErrNotFound, op, "no such secret")
	}
	if !meta.CASRequired {
		mount, err := s.mountCASRequired(ctx, op, sc.WorkloadScope)
		if err != nil {
			return secretstore.Metadata{}, err
		}
		meta.CASRequired = mount
	}
	return meta, nil
}

type writeResponse struct {
	Data *struct {
		Version int64 `json:"version"`
	} `json:"data"`
}

// Write is a check-and-set write. It is refused (ErrMisconfigured) unless the
// mount or the path enforces cas_required, so no other writer can bypass the
// check-and-set discipline this adapter relies on.
func (s *Store) Write(ctx context.Context, sc secretstore.StoreContext, ref secretstore.SecretRef, value secretstore.RuntimeSecret, casVersion int64) (int64, error) {
	op := secretstore.OpWrite
	if err := sc.Validate(op); err != nil {
		return 0, err
	}
	if casVersion < 0 {
		return 0, secretstore.Errorf(secretstore.ErrInvalidRequest, op, "negative check-and-set version")
	}
	raw := value.UnsafeBytes()
	if len(raw) == 0 || !utf8.Valid(raw) {
		return 0, secretstore.Errorf(secretstore.ErrInvalidRequest, op, "the store holds non-empty UTF-8 text only")
	}
	dataPath, err := s.apiPath(op, ref, "data")
	if err != nil {
		return 0, err
	}
	metadataPath, _ := s.apiPath(op, ref, "metadata")

	enforced, err := s.mountCASRequired(ctx, op, sc.WorkloadScope)
	if err != nil {
		return 0, err
	}
	if !enforced {
		meta, found, err := s.pathMetadata(ctx, op, sc.WorkloadScope, metadataPath)
		if err != nil {
			return 0, err
		}
		enforced = found && meta.CASRequired
	}
	if !enforced {
		return 0, secretstore.Errorf(secretstore.ErrMisconfigured, op, "cas_required is not set on the mount or the path")
	}

	body, err := json.Marshal(struct {
		Options map[string]int64  `json:"options"`
		Data    map[string]string `json:"data"`
	}{
		Options: map[string]int64{"cas": casVersion},
		Data:    map[string]string{valueKey: string(raw)},
	})
	if err != nil {
		return 0, secretstore.Errorf(secretstore.ErrInvalidRequest, op, "encode the write request")
	}
	defer clear(body)
	status, response, err := s.client.call(ctx, op, sc.WorkloadScope, http.MethodPost, dataPath, body)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, mapStatus(op, status, response)
	}
	var decoded writeResponse
	if err := json.Unmarshal(response, &decoded); err != nil || decoded.Data == nil {
		return 0, secretstore.Errorf(secretstore.ErrUnavailable, op, "malformed write response")
	}
	if decoded.Data.Version != casVersion+1 {
		return 0, secretstore.Errorf(secretstore.ErrMisconfigured, op, "backend created an unexpected version")
	}
	return decoded.Data.Version, nil
}

// Retire destroys one version permanently (KV v2 destroy). Destroying a
// version that does not exist succeeds, as KV v2 itself answers.
func (s *Store) Retire(ctx context.Context, sc secretstore.StoreContext, ref secretstore.SecretRef, version int64) error {
	op := secretstore.OpRetire
	if err := sc.Validate(op); err != nil {
		return err
	}
	if version <= 0 {
		return secretstore.Errorf(secretstore.ErrInvalidRequest, op, "an explicit secret version is required")
	}
	path, err := s.apiPath(op, ref, "destroy")
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string][]int64{"versions": {version}})
	status, raw, err := s.client.call(ctx, op, sc.WorkloadScope, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return mapStatus(op, status, raw)
	}
	return nil
}

// CheckReady logs in as the readiness scope and reads the KV mount
// configuration, which must enforce cas_required (ADR-0011 amendment, KV v2
// rule 1).
func (s *Store) CheckReady(ctx context.Context) error {
	op := secretstore.Op("readiness")
	enforced, err := s.mountCASRequired(ctx, op, s.client.cfg.ReadinessScope)
	if err != nil {
		return err
	}
	if !enforced {
		return secretstore.Errorf(secretstore.ErrMisconfigured, op, "the KV mount does not set cas_required=true")
	}
	return nil
}
