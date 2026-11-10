package faults

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type supportRefKey struct{}

// WithSupportRef returns ctx carrying ref as the support reference.
func WithSupportRef(ctx context.Context, ref string) context.Context {
	return context.WithValue(ctx, supportRefKey{}, ref)
}

// SupportRef is the support reference ctx carries, or "".
func SupportRef(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ref, _ := ctx.Value(supportRefKey{}).(string)
	return ref
}

// NewSupportRef is a fresh server-generated reference: "sr-" and 16
// random hex digits. It is never taken from a client (FU-299 C1).
func NewSupportRef() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("faults: no randomness for a support reference: " + err.Error())
	}
	return "sr-" + hex.EncodeToString(raw[:])
}
