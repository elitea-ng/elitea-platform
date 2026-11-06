package runtimecomposition

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/egresstoken"
)

func TestNewBrowserContextResolverWithoutAContentPoolServesNoField(t *testing.T) {
	var minter *egresstoken.Minter
	dependencies := Dependencies{}
	dependencies.Workspace.Egress.Minter = minter
	resolver, err := newBrowserContextResolver(dependencies)
	if err != nil || resolver != nil {
		t.Fatalf("resolver=%v err=%v", resolver, err)
	}
	if !isTypedNilMinter(minter) {
		t.Fatal("a typed nil minter reads as a minter")
	}
}
