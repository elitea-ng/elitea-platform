package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/discovery"
)

// deploymentKindEnv names who operates this deployment (ADR-0025 decision 1):
// "saas" or "self_hosted". It is published in the discovery document. It is
// process configuration rather than an admin section because an in-product
// administrator cannot change who operates the deployment, and because every
// replica must answer the anonymous document identically.
const deploymentKindEnv = "ELITEA_DEPLOYMENT_KIND"

// deploymentKindFromEnv reads ELITEA_DEPLOYMENT_KIND. Unset or blank means
// self_hosted (the chart's default); any other value refuses boot, the same
// rule the mailer's settings follow: a typo must not silently publish the
// wrong kind to every client.
func deploymentKindFromEnv(lookup func(string) (string, bool)) (string, error) {
	if lookup == nil {
		return "", errors.New("deployment kind environment lookup is required")
	}
	raw, ok := lookup(deploymentKindEnv)
	value := strings.TrimSpace(raw)
	if !ok || value == "" {
		return discovery.DeploymentKindSelfHosted, nil
	}
	switch value {
	case discovery.DeploymentKindSaaS, discovery.DeploymentKindSelfHosted:
		return value, nil
	}
	return "", fmt.Errorf("%s=%q: want %q or %q", deploymentKindEnv, raw,
		discovery.DeploymentKindSaaS, discovery.DeploymentKindSelfHosted)
}
