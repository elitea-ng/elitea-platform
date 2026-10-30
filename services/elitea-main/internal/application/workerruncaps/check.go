package workerruncaps

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerapprovalpolicy"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// CodeCapAboveTier is the typed 422 of an agent save whose meta.run_caps
// raises a cap above the platform's cap for the agent's tier.
const CodeCapAboveTier = "RUN_CAP_ABOVE_TIER"

// CodeCapInvalid is the 422 of a meta.run_caps value that is not a cap.
const CodeCapInvalid = "RUN_CAP_INVALID"

// CapRaiseError is a refused author cap.
type CapRaiseError struct {
	Code string
	// Cap is the refused cap.
	Cap string
	// Tier is the agent's risk tier and TierCap the platform's cap at it.
	Tier    agentlifecycle.Tier
	TierCap float64
	// Requested is the author's value.
	Requested float64
}

func (e *CapRaiseError) Error() string {
	if e.Code == CodeCapInvalid {
		return fmt.Sprintf("meta.run_caps.%s must be a positive number within the cap's range "+
			"(a whole number for a count, at most four decimals for max_spend)", e.Cap)
	}
	return fmt.Sprintf("meta.run_caps.%s is %g, above the platform cap %g for %s tier agents: "+
		"an agent can lower a run cap, never raise it", e.Cap, e.Requested, e.TierCap, e.Tier)
}

// Checker refuses an author's caps that raise a platform cap. It is the
// agent save path's check.
type Checker struct {
	settings SettingsReader
	tiers    workerapprovalpolicy.TierReader
}

// NewChecker builds the checker.
func NewChecker(settings SettingsReader, tiers workerapprovalpolicy.TierReader) (*Checker, error) {
	if settings == nil || tiers == nil {
		return nil, errors.New("worker run caps: the settings and tier readers are required")
	}
	return &Checker{settings: settings, tiers: tiers}, nil
}

// Check checks meta.run_caps of an agent version about to be saved. meta is
// the version's meta object (nil when the request carries none). An agent
// without an id yet (a create) has the tier of a new agent, low. A
// *CapRaiseError is the typed 422; another error is a source that could not
// be read.
func (c *Checker) Check(ctx context.Context, projectID, applicationID int64, meta map[string]any) error {
	raw, present := meta[MetaKey]
	if !present || raw == nil {
		return nil
	}
	author, invalid := AuthorCaps(raw)
	if len(invalid) > 0 {
		slices.Sort(invalid)
		return &CapRaiseError{Code: CodeCapInvalid, Cap: invalid[0]}
	}
	settings, err := c.settings.Get(ctx)
	if err != nil {
		return fmt.Errorf("read the platform run caps: %w", err)
	}
	tier := agentlifecycle.TierLow
	if projectID > 0 && applicationID > 0 {
		if tier, err = workerapprovalpolicy.ReadTier(ctx, c.tiers, projectID, applicationID); err != nil {
			return err
		}
	}
	return AboveTier(settings.For(tier), author, tier)
}

// AboveTier answers the first author cap above the tier's cap, or nil. A
// tier cap of 0 is no cap: any author value lowers it.
func AboveTier(tier, author platformconfig.TierRunCaps, name agentlifecycle.Tier) error {
	for _, cap := range platformconfig.RunCapNames {
		limit, requested := tier.Value(cap), author.Value(cap)
		if requested > 0 && limit > 0 && requested > limit {
			return &CapRaiseError{Code: CodeCapAboveTier, Cap: cap, Tier: name, TierCap: limit, Requested: requested}
		}
	}
	return nil
}
