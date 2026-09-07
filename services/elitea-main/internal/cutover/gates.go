package cutover

import (
	"context"
	"fmt"
	"time"
)

type GateChecker struct {
	tracker         *Tracker
	healthCheck     func(ctx context.Context) error
	shadowMatchRate func(ctx context.Context) (rate float64, total int, err error)
	errorRateCheck  func(ctx context.Context, endpoint string, window time.Duration) (float64, error)
}

type GateConfig struct {
	MinShadowSamples   int
	MinShadowMatchRate float64
	MaxErrorRate       float64
	ErrorRateWindow    time.Duration
}

func DefaultGateConfig() GateConfig {
	return GateConfig{
		MinShadowSamples:   10,
		MinShadowMatchRate: 0.95,
		MaxErrorRate:       0.01,
		ErrorRateWindow:    5 * time.Minute,
	}
}

type GateResult struct {
	Passed bool
	Gates  []GateOutcome
}

type GateOutcome struct {
	Name    string
	Passed  bool
	Message string
}

func (g *GateChecker) Check(ctx context.Context, endpoint, targetState string, cfg GateConfig) GateResult {
	result := GateResult{Passed: true}

	if g.healthCheck != nil {
		outcome := GateOutcome{Name: "health"}
		if err := g.healthCheck(ctx); err != nil {
			outcome.Passed = false
			outcome.Message = fmt.Sprintf("health check failed: %v", err)
			result.Passed = false
		} else {
			outcome.Passed = true
			outcome.Message = "healthy"
		}
		result.Gates = append(result.Gates, outcome)
	}

	if (targetState == StateCanary || targetState == StateGo) && g.shadowMatchRate != nil {
		outcome := GateOutcome{Name: "shadow_parity"}
		rate, total, err := g.shadowMatchRate(ctx)
		if err != nil {
			outcome.Passed = false
			outcome.Message = fmt.Sprintf("shadow stats unavailable: %v", err)
			result.Passed = false
		} else if total < cfg.MinShadowSamples {
			outcome.Passed = false
			outcome.Message = fmt.Sprintf("insufficient samples: %d < %d", total, cfg.MinShadowSamples)
			result.Passed = false
		} else if rate < cfg.MinShadowMatchRate {
			outcome.Passed = false
			outcome.Message = fmt.Sprintf("match rate %.1f%% < %.1f%%", rate*100, cfg.MinShadowMatchRate*100)
			result.Passed = false
		} else {
			outcome.Passed = true
			outcome.Message = fmt.Sprintf("match rate %.1f%% (%d samples)", rate*100, total)
		}
		result.Gates = append(result.Gates, outcome)
	}

	if targetState == StateGo && g.errorRateCheck != nil {
		outcome := GateOutcome{Name: "error_rate"}
		errRate, err := g.errorRateCheck(ctx, endpoint, cfg.ErrorRateWindow)
		if err != nil {
			outcome.Passed = false
			outcome.Message = fmt.Sprintf("error rate check failed: %v", err)
			result.Passed = false
		} else if errRate > cfg.MaxErrorRate {
			outcome.Passed = false
			outcome.Message = fmt.Sprintf("error rate %.2f%% > %.2f%%", errRate*100, cfg.MaxErrorRate*100)
			result.Passed = false
		} else {
			outcome.Passed = true
			outcome.Message = fmt.Sprintf("error rate %.2f%%", errRate*100)
		}
		result.Gates = append(result.Gates, outcome)
	}

	return result
}

type GateOption func(*GateChecker)

func WithHealthCheck(fn func(ctx context.Context) error) GateOption {
	return func(g *GateChecker) { g.healthCheck = fn }
}

func WithShadowMatchRate(fn func(ctx context.Context) (float64, int, error)) GateOption {
	return func(g *GateChecker) { g.shadowMatchRate = fn }
}

func WithErrorRateCheck(fn func(ctx context.Context, endpoint string, window time.Duration) (float64, error)) GateOption {
	return func(g *GateChecker) { g.errorRateCheck = fn }
}

func NewGateChecker(tracker *Tracker, opts ...GateOption) *GateChecker {
	g := &GateChecker{tracker: tracker}
	for _, o := range opts {
		o(g)
	}
	return g
}
