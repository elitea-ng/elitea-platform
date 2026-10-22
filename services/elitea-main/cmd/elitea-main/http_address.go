package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const defaultHTTPAddress = ":8080"

func configuredHTTPAddress(lookup func(string) (string, bool)) (string, error) {
	if lookup == nil {
		return "", errors.New("HTTP address environment lookup is required")
	}
	address, ok := lookup("ELITEA_HTTP_ADDRESS")
	if !ok || address == "" {
		address = defaultHTTPAddress
	}
	if len(address) > 256 || strings.ContainsAny(address, "\r\n\x00") {
		return "", errors.New("ELITEA_HTTP_ADDRESS is invalid")
	}
	_, portValue, err := net.SplitHostPort(address)
	if err != nil {
		return "", errors.New("ELITEA_HTTP_ADDRESS must include a numeric TCP port")
	}
	port, err := strconv.Atoi(portValue)
	if err != nil || port <= 0 || port > 65535 || strconv.Itoa(port) != portValue {
		return "", errors.New("ELITEA_HTTP_ADDRESS must include a numeric TCP port")
	}
	return address, nil
}

// healthcheckPaths are the probe paths `elitea-main -healthcheck [path]` may
// read. /readyz is the default, which is what the compose healthchecks rely on.
//
// The argument exists for the chart (#1359 P3). There elitea-main is the
// browser edge, and internal/api/webedge answers /readyz and /startupz with a
// 404 to every caller that is not loopback, so a kubelet httpGet probe, which
// arrives from the node's address, would never see the pod ready. The chart
// runs this binary as an exec probe instead: the request then comes from
// loopback inside the container, exactly as the compose healthcheck's does.
//
// It is a closed list rather than any path, so a probe argument cannot turn
// the binary into a general HTTP client against its own listener.
var healthcheckPaths = map[string]bool{"/readyz": true, "/startupz": true, "/healthz": true}

// healthcheckURL names the loopback URL `-healthcheck` requests. args are the
// command-line arguments after `-healthcheck`: none, or one probe path.
func healthcheckURL(lookup func(string) (string, bool), args ...string) (string, error) {
	path := "/readyz"
	switch len(args) {
	case 0:
	case 1:
		if !healthcheckPaths[args[0]] {
			return "", fmt.Errorf("-healthcheck: path %q is not one of /readyz, /startupz, /healthz", args[0])
		}
		path = args[0]
	default:
		return "", errors.New("-healthcheck takes at most one path")
	}
	address, err := configuredHTTPAddress(lookup)
	if err != nil {
		return "", err
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: path}).String(), nil
}
