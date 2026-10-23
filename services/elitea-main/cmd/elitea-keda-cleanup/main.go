// elitea-keda-cleanup deletes a release's KEDA objects before the release
// itself is uninstalled. The Helm chart runs it as a pre-delete hook when the
// release installs KEDA (keda.enabled).
//
// WHY. `helm uninstall` deletes KEDA's operator and its CRDs in the same pass
// as the release's ScaledObjects. Each ScaledObject carries KEDA's finalizer,
// and only the operator removes it. When the operator goes first, the
// ScaledObjects never finish deleting, the CRD stays in Terminating, and so
// does the namespace. A pre-delete hook runs while the operator still runs:
// it deletes the ScaledObjects and TriggerAuthentications that carry the
// release's instance label, and waits until the operator has removed them.
//
// When the wait ends with objects left (the operator is not running), it
// removes their finalizers, because nothing else will. The HPA that KEDA made
// for a ScaledObject has the ScaledObject as its owner, so the garbage
// collector removes it.
//
// It talks to the Kubernetes API with the pod's service account token over
// the in-cluster TLS endpoint. It needs delete, list and patch on
// scaledobjects and triggerauthentications in its own namespace, and nothing
// else (templates/keda-cleanup.yaml).
//
//	ELITEA_KEDA_CLEANUP_NAMESPACE  the release namespace (the downward API)
//	ELITEA_KEDA_CLEANUP_SELECTOR   the label selector (app.kubernetes.io/instance=<release>)
//	ELITEA_KEDA_CLEANUP_TIMEOUT    how long to wait for the operator (default 120s)
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	kedaGroupVersion  = "keda.sh/v1alpha1"
	defaultTimeout    = 120 * time.Second
)

// resources are the KEDA kinds a release of this chart creates.
var resources = []string{"scaledobjects", "triggerauthentications"}

// pollInterval is how often the wait reads what is left (a variable for the tests).
var pollInterval = 2 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := configFromEnv(os.LookupEnv)
	if err != nil {
		logger.Error("elitea-keda-cleanup: configuration", "error", err)
		os.Exit(2)
	}
	api, err := inCluster(os.LookupEnv, serviceAccountDir)
	if err != nil {
		logger.Error("elitea-keda-cleanup: the Kubernetes API", "error", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout+30*time.Second)
	defer cancel()
	if err := cleanup(ctx, api, cfg, logger); err != nil {
		logger.Error("elitea-keda-cleanup failed", "error", err)
		os.Exit(1)
	}
}

type config struct {
	namespace string
	selector  string
	timeout   time.Duration
}

func configFromEnv(lookup func(string) (string, bool)) (config, error) {
	var cfg config
	cfg.namespace, _ = lookup("ELITEA_KEDA_CLEANUP_NAMESPACE")
	cfg.selector, _ = lookup("ELITEA_KEDA_CLEANUP_SELECTOR")
	cfg.namespace = strings.TrimSpace(cfg.namespace)
	cfg.selector = strings.TrimSpace(cfg.selector)
	if cfg.namespace == "" {
		return cfg, errors.New("ELITEA_KEDA_CLEANUP_NAMESPACE is required")
	}
	// An empty selector would select every KEDA object in the namespace,
	// including those of other releases.
	if cfg.selector == "" {
		return cfg, errors.New("ELITEA_KEDA_CLEANUP_SELECTOR is required: an empty selector selects other releases' objects too")
	}
	cfg.timeout = defaultTimeout
	if raw, ok := lookup("ELITEA_KEDA_CLEANUP_TIMEOUT"); ok && strings.TrimSpace(raw) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("ELITEA_KEDA_CLEANUP_TIMEOUT must be a positive Go duration, got %q", raw)
		}
		cfg.timeout = d
	}
	return cfg, nil
}

// client is the minimum of the Kubernetes API this tool needs.
type client struct {
	base  string
	token string
	http  *http.Client
}

func inCluster(lookup func(string) (string, bool), dir string) (*client, error) {
	host, _ := lookup("KUBERNETES_SERVICE_HOST")
	port, _ := lookup("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set: this runs in a pod only")
	}
	token, err := os.ReadFile(dir + "/token")
	if err != nil {
		return nil, fmt.Errorf("read the service account token: %w", err)
	}
	caPEM, err := os.ReadFile(dir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read the cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("the cluster CA file holds no certificate")
	}
	return &client{
		base:  "https://" + net.JoinHostPort(host, port),
		token: strings.TrimSpace(string(token)),
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}, nil
}

// errNoResource is a 404 on the collection: the CRD is not installed, so
// there is nothing of that kind to delete.
var errNoResource = errors.New("the resource type is not served")

func (c *client) do(ctx context.Context, method, path string, body io.Reader, contentType string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	switch {
	case response.StatusCode == http.StatusNotFound:
		return nil, errNoResource
	case response.StatusCode >= 300:
		return nil, fmt.Errorf("%s %s: %d %s", method, path, response.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func collection(namespace, resource, selector string) string {
	return fmt.Sprintf("/apis/%s/namespaces/%s/%s?labelSelector=%s",
		kedaGroupVersion, url.PathEscape(namespace), resource, url.QueryEscape(selector))
}

// list returns the names of the objects of one kind the selector matches.
func (c *client) list(ctx context.Context, cfg config, resource string) ([]string, error) {
	data, err := c.do(ctx, http.MethodGet, collection(cfg.namespace, resource, cfg.selector), nil, "")
	if err != nil {
		return nil, err
	}
	var document struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("list %s: %w", resource, err)
	}
	names := make([]string, 0, len(document.Items))
	for _, item := range document.Items {
		names = append(names, item.Metadata.Name)
	}
	return names, nil
}

// cleanup deletes the selected objects, waits for the operator to finish
// them, and removes the finalizers of what is left at the deadline.
func cleanup(ctx context.Context, c *client, cfg config, logger *slog.Logger) error {
	served := map[string]bool{}
	for _, resource := range resources {
		_, err := c.do(ctx, http.MethodDelete, collection(cfg.namespace, resource, cfg.selector), nil, "")
		switch {
		case errors.Is(err, errNoResource):
			logger.Info("KEDA does not serve this kind; nothing to delete", "resource", resource)
		case err != nil:
			return fmt.Errorf("delete %s: %w", resource, err)
		default:
			served[resource] = true
			logger.Info("deletion requested", "resource", resource, "selector", cfg.selector)
		}
	}
	deadline := time.Now().Add(cfg.timeout)
	for {
		left := map[string][]string{}
		for resource := range served {
			names, err := c.list(ctx, cfg, resource)
			if err != nil && !errors.Is(err, errNoResource) {
				return err
			}
			if len(names) > 0 {
				left[resource] = names
			}
		}
		if len(left) == 0 {
			logger.Info("the release's KEDA objects are gone")
			return nil
		}
		if time.Now().After(deadline) {
			logger.Warn("the KEDA operator did not finish the objects in time; removing their finalizers", "left", left)
			return removeFinalizers(ctx, c, cfg, left)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func removeFinalizers(ctx context.Context, c *client, cfg config, left map[string][]string) error {
	patch := `{"metadata":{"finalizers":null}}`
	for resource, names := range left {
		for _, name := range names {
			path := fmt.Sprintf("/apis/%s/namespaces/%s/%s/%s", kedaGroupVersion, url.PathEscape(cfg.namespace), resource, url.PathEscape(name))
			if _, err := c.do(ctx, http.MethodPatch, path, strings.NewReader(patch), "application/merge-patch+json"); err != nil && !errors.Is(err, errNoResource) {
				return fmt.Errorf("remove the finalizers of %s/%s: %w", resource, name, err)
			}
		}
	}
	return nil
}
