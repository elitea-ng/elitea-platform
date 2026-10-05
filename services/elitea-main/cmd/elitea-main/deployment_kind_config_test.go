package main

import (
	"io"
	"log/slog"
	"testing"
)

func TestDeploymentKindFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		want    string
		wantErr bool
	}{
		{name: "unset", env: map[string]string{}, want: "self_hosted"},
		{name: "empty", env: map[string]string{deploymentKindEnv: "  "}, want: "self_hosted"},
		{name: "saas", env: map[string]string{deploymentKindEnv: "saas"}, want: "saas"},
		{name: "self_hosted", env: map[string]string{deploymentKindEnv: " self_hosted "}, want: "self_hosted"},
		{name: "typo refuses boot", env: map[string]string{deploymentKindEnv: "SaaS"}, wantErr: true},
		{name: "other refuses boot", env: map[string]string{deploymentKindEnv: "self-hosted"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := deploymentKindFromEnv(envLookup(tc.env))
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got %q, %v; want %q, err=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
	if _, err := deploymentKindFromEnv(nil); err == nil {
		t.Fatal("nil lookup accepted")
	}
}

func TestPublicOriginFromEnv(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "unset derives from the request", env: map[string]string{}, want: ""},
		{name: "set", env: map[string]string{"DEPLOYMENT_URL": "https://Elitea.Example.com/"}, want: "https://elitea.example.com"},
		{name: "path is dropped", env: map[string]string{"DEPLOYMENT_URL": "https://elitea.example.com/base"}, want: "https://elitea.example.com"},
		{name: "invalid is logged, not fatal", env: map[string]string{"DEPLOYMENT_URL": "elitea.example.com"}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := publicOriginFromEnv(envLookup(tc.env), quiet)
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := publicOriginFromEnv(nil, quiet); err == nil {
		t.Fatal("nil lookup accepted")
	}
}
