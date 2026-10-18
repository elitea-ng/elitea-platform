package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaultsNeedOnlyTheKeys(t *testing.T) {
	c, err := FromEnv(env(map[string]string{Prefix + "PUBLIC_KEYS": "/etc/egress-proxy/keys"}))
	if err != nil {
		t.Fatal(err)
	}
	d := Defaults()
	if c.ListenAddr != ":3128" || c.AdminAddr != ":9090" || c.RunMaxConnections != d.RunMaxConnections ||
		c.RunMaxBytes != d.RunMaxBytes || c.IdleTimeout != d.IdleTimeout || c.LogLevel != slog.LevelInfo {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestEveryVariableIsRead(t *testing.T) {
	c, err := FromEnv(env(map[string]string{
		Prefix + "PUBLIC_KEYS":             "/k",
		Prefix + "LISTEN_ADDR":             ":8080",
		Prefix + "ADMIN_ADDR":              ":9100",
		Prefix + "KEYS_RELOAD_INTERVAL":    "0s",
		Prefix + "DENY_CIDRS":              "10.96.0.0/12, 10.244.0.0/16,,fd00:10::/108",
		Prefix + "MAX_CONNECTIONS":         "10",
		Prefix + "MAX_HEADER_BYTES":        "20000",
		Prefix + "READ_HEADER_TIMEOUT":     "3s",
		Prefix + "DIAL_TIMEOUT":            "4s",
		Prefix + "RUN_MAX_CONNECTIONS":     "5",
		Prefix + "RUN_MAX_BYTES":           "12345",
		Prefix + "IDLE_TIMEOUT":            "6s",
		Prefix + "MAX_CONNECTION_LIFETIME": "7m",
		Prefix + "SUMMARY_INTERVAL":        "8s",
		Prefix + "DRAIN_DELAY":             "0s",
		Prefix + "SHUTDOWN_TIMEOUT":        "9s",
		Prefix + "LOG_LEVEL":               "debug",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":8080" || c.AdminAddr != ":9100" || c.KeysReloadInterval != 0 || len(c.DenyCIDRs) != 3 ||
		c.MaxConnections != 10 || c.MaxHeaderBytes != 20000 || c.ReadHeaderTimeout != 3*time.Second ||
		c.DialTimeout != 4*time.Second || c.RunMaxConnections != 5 || c.RunMaxBytes != 12345 ||
		c.IdleTimeout != 6*time.Second || c.MaxConnectionLifetime != 7*time.Minute || c.SummaryInterval != 8*time.Second ||
		c.DrainDelay != 0 || c.ShutdownTimeout != 9*time.Second || c.LogLevel != slog.LevelDebug {
		t.Fatalf("parsed: %+v", c)
	}
}

func TestBadValuesAreErrorsNotDefaults(t *testing.T) {
	_, err := FromEnv(env(map[string]string{
		Prefix + "RUN_MAX_CONNECTIONS": "-1",
		Prefix + "RUN_MAX_BYTES":       "lots",
		Prefix + "IDLE_TIMEOUT":        "0s",
		Prefix + "DENY_CIDRS":          "10.0.0.0",
		Prefix + "MAX_HEADER_BYTES":    "4096",
		Prefix + "LOG_LEVEL":           "loud",
	}))
	if err == nil {
		t.Fatal("bad values must fail")
	}
	for _, want := range []string{"PUBLIC_KEYS is required", "RUN_MAX_CONNECTIONS", "RUN_MAX_BYTES", "IDLE_TIMEOUT", "DENY_CIDRS", "MAX_HEADER_BYTES", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s: %v", want, err)
		}
	}
}

func TestAuditConfigIsAllOrNothing(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(key string) string {
			if key == Prefix+"PUBLIC_KEYS" {
				return "/keys"
			}
			return values[key]
		}
	}
	c, err := FromEnv(env(map[string]string{
		Prefix + "AUDIT_TARGET": "elitea-main:9443", Prefix + "AUDIT_CA_FILE": "/ca", Prefix + "AUDIT_CERT_FILE": "/crt",
		Prefix + "AUDIT_KEY_FILE": "/key", Prefix + "REPLICA_ID": "pod-1",
	}))
	if err != nil || c.Audit.Target != "elitea-main:9443" || c.Audit.ReplicaID != "pod-1" || c.Audit.Timeout <= 0 {
		t.Fatalf("%+v %v", c.Audit, err)
	}
	for name, values := range map[string]map[string]string{
		"target without material": {Prefix + "AUDIT_TARGET": "elitea-main:9443"},
		"material without target": {Prefix + "AUDIT_CA_FILE": "/ca"},
		"target without port":     {Prefix + "AUDIT_TARGET": "elitea-main", Prefix + "AUDIT_CA_FILE": "/ca", Prefix + "AUDIT_CERT_FILE": "/c", Prefix + "AUDIT_KEY_FILE": "/k"},
	} {
		if _, err := FromEnv(env(values)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
