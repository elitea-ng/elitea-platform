package runtime

import (
	"bytes"
	"encoding/json"
	"io"
)

// SuccessfulSnapshotReceipt checks the supervisor's immutable terminal envelope,
// then compares the compiler's output to the independently captured descriptor.
// Stdout alone never establishes export authority or confirmed runtime cleanup.
func SuccessfulSnapshotReceipt(raw []byte, captured []byte) error {
	if len(raw) == 0 || len(raw) > 512*1024 || uniqueSnapshotJSON(raw) != nil {
		return ErrSnapshotInvalid
	}
	var receipt struct {
		Revision uint32 `json:"revision"`
		Status   string `json:"status"`
		ExitCode *int   `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if strictSnapshotJSON(raw, &receipt) != nil || receipt.Revision != 1 || receipt.Status != "completed" || receipt.ExitCode == nil || *receipt.ExitCode != 0 {
		return ErrSnapshotInvalid
	}
	stdout := []byte(receipt.Stdout)
	if len(stdout) > SnapshotDescriptorLimit+128 || uniqueSnapshotJSON(stdout) != nil {
		return ErrSnapshotInvalid
	}
	var output struct {
		Revision uint32                 `json:"revision"`
		Artifact RustSnapshotDescriptor `json:"compiled_artifact"`
	}
	if strictSnapshotJSON(stdout, &output) != nil || output.Revision != 1 {
		return ErrSnapshotInvalid
	}
	actual, err := SnapshotJSON(output.Artifact)
	if err != nil || !bytes.Equal(actual, captured) {
		return ErrSnapshotInvalid
	}
	return nil
}
func strictSnapshotJSON(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil || d.Decode(new(any)) != io.EOF {
		return ErrSnapshotInvalid
	}
	return nil
}
func uniqueSnapshotJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return ErrSnapshotInvalid
		}
		token, err := d.Token()
		if err != nil {
			return ErrSnapshotInvalid
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]struct{}{}
			for d.More() {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok {
					return ErrSnapshotInvalid
				}
				if _, exists := seen[name]; exists {
					return ErrSnapshotInvalid
				}
				seen[name] = struct{}{}
				if value(depth+1) != nil {
					return ErrSnapshotInvalid
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return ErrSnapshotInvalid
			}
		case '[':
			for d.More() {
				if value(depth+1) != nil {
					return ErrSnapshotInvalid
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return ErrSnapshotInvalid
			}
		default:
			return ErrSnapshotInvalid
		}
		return nil
	}
	if value(0) != nil {
		return ErrSnapshotInvalid
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrSnapshotInvalid
	}
	return nil
}
