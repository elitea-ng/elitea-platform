package configurations

import (
	"errors"
	"sort"

	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

var (
	ErrInvalidValidationCommand  = errors.New("invalid configuration validation command")
	ErrInvalidValidationResult   = errors.New("invalid configuration validation result")
	ErrValidationBindingMismatch = errors.New("configuration validation binding mismatch")
)

const (
	MaxValidationIssues          = 64
	MaxSafeValidationStringBytes = 256
)

type ValidationCommand struct {
	ConfigurationRevisionID string
	ConfigurationType       string
	CatalogRevision         string
	CatalogDigest           runtimedomain.Digest
	SchemaID                string
	SchemaRevision          string
	SchemaDigest            runtimedomain.Digest
	SettingsEntryID         string
}

func (c ValidationCommand) Validate() error {
	if c.ConfigurationRevisionID == "" || c.ConfigurationType == "" || c.CatalogRevision == "" || c.CatalogDigest.IsZero() || c.SchemaID == "" || c.SchemaRevision == "" || c.SchemaDigest.IsZero() || c.SettingsEntryID == "" {
		return ErrInvalidValidationCommand
	}
	return nil
}

type ValidationBinding struct {
	Command               ValidationCommand
	InputBundleID         string
	InputBundleDigest     runtimedomain.Digest
	SettingsEntryVersion  string
	SettingsContentDigest runtimedomain.Digest
}

func (b ValidationBinding) Validate() error {
	if err := b.Command.Validate(); err != nil {
		return err
	}
	if b.InputBundleID == "" || b.InputBundleDigest.IsZero() || b.SettingsEntryVersion == "" || b.SettingsContentDigest.IsZero() {
		return ErrInvalidValidationResult
	}
	return nil
}

type ValidationIssue struct {
	Code        string
	JSONPointer string
	SafeMessage string
}

type ValidationResult struct {
	Binding ValidationBinding
	Valid   bool
	Issues  []ValidationIssue
}

var canonicalValidationIssueMessages = map[string]string{
	"VALUE_NOT_ALLOWED":     "Value is not one of the allowed choices.",
	"REQUIRED_FIELD":        "A required value is missing.",
	"UNKNOWN_FIELD":         "This field is not allowed.",
	"INVALID_CONFIGURATION": "Configuration fields are inconsistent.",
	"VALUE_OUT_OF_RANGE":    "Value is outside the allowed range.",
	"INVALID_VALUE":         "Value does not satisfy the configuration schema.",
}

func CanonicalValidationIssueMessage(code string) (string, bool) {
	message, ok := canonicalValidationIssueMessages[code]
	return message, ok
}

func (r ValidationResult) Validate() error {
	if err := r.Binding.Validate(); err != nil {
		return err
	}
	if r.Valid && len(r.Issues) != 0 {
		return ErrInvalidValidationResult
	}
	if !r.Valid && len(r.Issues) == 0 {
		return ErrInvalidValidationResult
	}
	if len(r.Issues) > MaxValidationIssues {
		return ErrInvalidValidationResult
	}

	seen := make(map[string]struct{}, len(r.Issues))
	for i, issue := range r.Issues {
		canonicalMessage, registered := CanonicalValidationIssueMessage(issue.Code)
		if !registered || issue.SafeMessage != canonicalMessage || len(issue.Code) > MaxSafeValidationStringBytes || len(issue.JSONPointer) > MaxSafeValidationStringBytes || len(issue.SafeMessage) > MaxSafeValidationStringBytes {
			return ErrInvalidValidationResult
		}
		key := issue.Code + "\x00" + issue.JSONPointer
		if _, exists := seen[key]; exists {
			return ErrInvalidValidationResult
		}
		seen[key] = struct{}{}
		if i > 0 && issueLess(issue, r.Issues[i-1]) {
			return ErrInvalidValidationResult
		}
	}
	return nil
}

func issueLess(a, b ValidationIssue) bool {
	if a.JSONPointer != b.JSONPointer {
		return a.JSONPointer < b.JSONPointer
	}
	if a.Code != b.Code {
		return a.Code < b.Code
	}
	return a.SafeMessage < b.SafeMessage
}

func SortIssues(issues []ValidationIssue) {
	sort.SliceStable(issues, func(i, j int) bool { return issueLess(issues[i], issues[j]) })
}
