package workspacehandoff

import (
	"errors"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// Explicit includes and the environment harvest (ADR-0034 decision 5,
// amended 2026-10-15). Main re-applies the effective policy at send: the
// desktop applies it first, but a client is not a policy boundary.

var (
	// ErrIncludeNotAllowed: the policy does not allow explicit includes.
	ErrIncludeNotAllowed = errors.New("explicit include is not allowed")
	// ErrPathDenied: an include matches local_work_path_deny.
	ErrPathDenied = errors.New("an include matches the path deny list")
	// ErrEnvHarvestDisabled: the policy does not allow the harvest.
	ErrEnvHarvestDisabled = errors.New("environment harvest is disabled")
	// ErrEnvTooLarge: more than 200 variables or 64 KiB.
	ErrEnvTooLarge = errors.New("harvested environment too large")
	// ErrEnvNameDenied matches every EnvNameDeniedError.
	ErrEnvNameDenied = errors.New("environment variable name denied")
)

// EnvNameDeniedError names the variables the policy refuses.
type EnvNameDeniedError struct {
	Names []string
}

func (e *EnvNameDeniedError) Error() string { return "environment variable names denied" }

// Is lets errors.Is(err, ErrEnvNameDenied) match.
func (e *EnvNameDeniedError) Is(target error) bool { return target == ErrEnvNameDenied }

// PathDeniedError names the includes that match local_work_path_deny.
type PathDeniedError struct {
	Paths []string
}

func (e *PathDeniedError) Error() string { return "includes match the path deny list" }

// Is lets errors.Is(err, ErrPathDenied) match.
func (e *PathDeniedError) Is(target error) bool { return target == ErrPathDenied }

// MaxIncludes bounds the explicit includes of one send.
const MaxIncludes = 1000

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// checkIncludes applies the include rule: allowed by policy, and never a
// path_deny path.
func checkIncludes(includes []string, policy Policy) error {
	if len(includes) == 0 {
		return nil
	}
	if len(includes) > MaxIncludes {
		return ErrInvalid
	}
	if !policy.AllowUserInclude {
		return ErrIncludeNotAllowed
	}
	denied := []string{}
	for _, include := range includes {
		if include == "" || len(include) > 4096 || !utf8.ValidString(include) || strings.ContainsRune(include, 0) {
			return ErrInvalid
		}
		for _, pattern := range policy.PathDeny {
			if MatchPathPattern(pattern, include) {
				denied = append(denied, include)
				break
			}
		}
	}
	if len(denied) > 0 {
		return &PathDeniedError{Paths: denied}
	}
	return nil
}

// checkEnv applies the harvest rule: allowed by policy, names valid, none
// denied, and within the size caps. It answers the sorted names.
func checkEnv(env map[string]string, policy Policy) ([]string, error) {
	if len(env) == 0 {
		return []string{}, nil
	}
	if !policy.EnvHarvest {
		return nil, ErrEnvHarvestDisabled
	}
	if len(env) > platformconfig.MaxCloudExecutorEnvVars {
		return nil, ErrEnvTooLarge
	}
	total := 0
	names := make([]string, 0, len(env))
	for name, value := range env {
		if !envName.MatchString(name) || len(name) > 256 || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return nil, ErrInvalid
		}
		total += len(name) + len(value)
		names = append(names, name)
	}
	if total > platformconfig.MaxCloudExecutorEnvBytes {
		return nil, ErrEnvTooLarge
	}
	sort.Strings(names)
	denied := []string{}
	for _, name := range names {
		if EnvNameDenied(name, policy) {
			denied = append(denied, name)
		}
	}
	if len(denied) > 0 {
		return nil, &EnvNameDeniedError{Names: denied}
	}
	return names, nil
}

// EnvNameDenied reports a name the env_deny patterns match, or one outside a
// set env_allow.
func EnvNameDenied(name string, policy Policy) bool {
	for _, pattern := range policy.EnvDeny {
		if matched, err := path.Match(pattern, name); err == nil && matched {
			return true
		}
	}
	if policy.EnvAllow != nil {
		for _, pattern := range *policy.EnvAllow {
			if matched, err := path.Match(pattern, name); err == nil && matched {
				return false
			}
		}
		return true
	}
	return false
}

// MatchPathPattern matches a workspace path ("./a/b", "a/b") against a
// gitignore-like pattern: `**` spans any number of directories, `*` and `?`
// stay within one segment, and a pattern without a slash matches the base name
// at any depth. As in gitignore, a pattern that matches a directory matches
// everything under it (`.ssh` and `secrets/` cover `secrets/a/key.pem`), and a
// pattern with a trailing slash matches directories only. Matching folds case
// on both sides: desktops are mostly on case-insensitive file systems, where
// `./.SSH/id_rsa` IS `.ssh/id_rsa`. A pattern anchored at the home directory
// (`~/…`) or absolute names nothing inside a workspace and matches no path.
func MatchPathPattern(pattern, name string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" || strings.HasPrefix(pattern, "~") || strings.HasPrefix(pattern, "/") {
		return false
	}
	name = strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(name, "./"), "/"))
	pattern = strings.TrimPrefix(pattern, "./")
	dirOnly := strings.HasSuffix(pattern, "/")
	pattern = strings.TrimRight(pattern, "/")
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "/") {
		pattern = "**/" + pattern
	}
	patternSegments := strings.Split(pattern, "/")
	nameSegments := strings.Split(strings.TrimRight(name, "/"), "/")
	// The path itself (unless the pattern names directories only), then each
	// directory that contains it.
	last := len(nameSegments)
	if dirOnly {
		last--
	}
	for end := last; end >= 1; end-- {
		if matchSegments(patternSegments, nameSegments[:end]) {
			return true
		}
	}
	return false
}

func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			rest := pattern[1:]
			if len(rest) == 0 {
				return true
			}
			for skip := 0; skip <= len(name); skip++ {
				if matchSegments(rest, name[skip:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		matched, err := path.Match(pattern[0], name[0])
		if err != nil || !matched {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}
