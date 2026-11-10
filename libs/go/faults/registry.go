// Package faults is the platform's error convention (FU-299): a stable code
// from one registry shared with the Rust side, a plain sentence, the ids, a
// support reference, the cause chain made safe by type, and a bounded stack
// for panics and unexpected errors.
//
// The registry is registry/*.json in this directory; libs/rust/faults reads
// the same files. A code exists only as a generated constant (codes_gen.go),
// so a typo does not compile and a new code cannot ship without its sentence.
package faults

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"sync"
)

//go:embed registry/*.json
var registryFiles embed.FS

// Code is a registered error code. Only the generated constants exist; a
// conversion from a string literal is refused by TestNoCodeIsMadeByHand.
type Code string

// String returns the code as written in the registry.
func (c Code) String() string { return string(c) }

// Sentence is the code's plain sentence: what failed and what happens next.
func (c Code) Sentence() string { return lookup(c).Sentence }

// Action is what an operator or user does about it.
func (c Code) Action() string { return lookup(c).Action }

// Entry is one registry entry.
type Entry struct {
	Code        string `json:"code"`
	Sentence    string `json:"sentence"`
	Action      string `json:"action"`
	Audience    string `json:"audience"`
	Retryable   string `json:"retryable"`
	Owner       string `json:"owner"`
	RuntimeKind string `json:"runtime_kind,omitempty"`
	WireCode    string `json:"wire_code,omitempty"`
	// File is the registry file the entry came from (not in the JSON).
	File string `json:"-"`
}

type registryFile struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

var (
	loadOnce sync.Once
	entries  []Entry
	byCode   map[string]Entry
	loadErr  error
)

func load() {
	byCode = make(map[string]Entry)
	names, err := fs.Glob(registryFiles, "registry/*.json")
	if err != nil {
		loadErr = err
		return
	}
	sort.Strings(names)
	for _, name := range names {
		raw, err := registryFiles.ReadFile(name)
		if err != nil {
			loadErr = err
			return
		}
		var file registryFile
		if err := json.Unmarshal(raw, &file); err != nil {
			loadErr = fmt.Errorf("%s: %w", name, err)
			return
		}
		for _, entry := range file.Entries {
			entry.File = path.Base(name)
			if _, dup := byCode[entry.Code]; dup {
				loadErr = fmt.Errorf("%s: code %q is registered twice", name, entry.Code)
				return
			}
			byCode[entry.Code] = entry
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Code < entries[j].Code })
}

// Registry returns every entry, sorted by code. It panics if the embedded
// registry is malformed, which TestRegistryIsWellFormed prevents shipping.
func Registry() []Entry {
	loadOnce.Do(load)
	if loadErr != nil {
		panic("faults: " + loadErr.Error())
	}
	return entries
}

func lookup(c Code) Entry {
	Registry()
	return byCode[string(c)]
}
