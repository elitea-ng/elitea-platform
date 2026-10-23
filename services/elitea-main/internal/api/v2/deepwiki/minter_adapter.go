package deepwiki

// The adapter from the platform's token minter to the facades' CallbackMinter.
// It moved to internal/providerhost/material with the rest of the rewrite
// mechanics — Inventory mints the same callback bearer — and this is the name
// DeepWiki's composition root already calls it by.

import (
	v2auth "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/material"
)

// NewAuthCallbackMinter wraps the platform's minter, and returns a true nil
// for a nil one rather than a typed nil the composition root's `!= nil` would
// accept.
func NewAuthCallbackMinter(minter *v2auth.CallbackTokenMinter) CallbackMinter {
	return material.NewCallbackMinter(minter)
}
