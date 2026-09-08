package runtimecomposition

// The origin elitea-main calls ITSELF on for the next-input-suggestion policy.
//
// The defect: the standalone stack set `https://elitea-main:8080`, which is
// this process's OWN listener, and this process serves cleartext there. Every
// chat turn logged
//
//	Get "https://elitea-main:8080/...": http: server gave HTTP response to
//	HTTPS client
//
// A deployment fault reported once per turn is a fault nobody fixes. The origin
// is now DERIVED from the listener when the variable is unset, and the one
// configuration that cannot work is refused at boot.

import (
	"strings"
	"testing"
)

func agentDispatchEnvironment() map[string]string {
	environment := validEnvironment()
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED"] = "true"
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM"] = "commands.v1.agent.execute.agents.shared.1.0"
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_CONSUMER_GROUP"] = "elitea-agent-worker-v1"
	environment["ELITEA_RUNTIME_AGENT_EXECUTION_STREAM_MAX_ENTRIES"] = "64"
	return environment
}

func TestSelfCallOriginIsDerivedFromTheListener(t *testing.T) {
	for name, testCase := range map[string]struct {
		address string
		want    string
	}{
		"the default listener":   {address: "", want: "http://127.0.0.1:8080"},
		"an explicit port":       {address: ":9090", want: "http://127.0.0.1:9090"},
		"a bound interface":      {address: "0.0.0.0:8081", want: "http://127.0.0.1:8081"},
		"an IPv6 bind":           {address: "[::]:8082", want: "http://127.0.0.1:8082"},
		"loopback already":       {address: "127.0.0.1:8083", want: "http://127.0.0.1:8083"},
		"a named interface bind": {address: "localhost:8084", want: "http://127.0.0.1:8084"},
	} {
		t.Run(name, func(t *testing.T) {
			environment := agentDispatchEnvironment()
			if testCase.address != "" {
				environment["ELITEA_HTTP_ADDRESS"] = testCase.address
			}
			config, err := ConfigFromEnv(mapLookup(environment))
			if err != nil {
				t.Fatalf("ConfigFromEnv: %v", err)
			}
			if config.CurrentMainBaseURL != testCase.want {
				t.Fatalf("CurrentMainBaseURL = %q, want %q", config.CurrentMainBaseURL, testCase.want)
			}
		})
	}
}

func TestSelfCallOriginRefusesHTTPSOnTheProcessOwnCleartextPort(t *testing.T) {
	// The exact standalone value, refused at BOOT instead of warned about on
	// every turn.
	environment := agentDispatchEnvironment()
	environment["ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL"] = "https://elitea-main:8080"
	_, err := ConfigFromEnv(mapLookup(environment))
	if err == nil {
		t.Fatal("an https origin on this process's cleartext listen port was accepted")
	}
	// The message has to name the fix, because the operator reading it is the
	// one who applies it.
	for _, expected := range []string{"cleartext", "unset"} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("error %q does not mention %q", err.Error(), expected)
		}
	}

	// The same origin on a port that is NOT this process's own is an edge that
	// terminates TLS, and stays valid.
	environment["ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL"] = "https://elitea-main:8443"
	if _, err := ConfigFromEnv(mapLookup(environment)); err != nil {
		t.Fatalf("a TLS front end on its own port was refused: %v", err)
	}
}

func TestSelfCallOriginKeepsTheExplicitEdgeOrigin(t *testing.T) {
	// centry-hybrid does not serve this route from this process: it aims the
	// call at the edge, which routes the path to legacy Centry. An explicit
	// https origin must still be honoured verbatim.
	environment := agentDispatchEnvironment()
	environment["ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL"] = "https://elitea-gateway"
	config, err := ConfigFromEnv(mapLookup(environment))
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if config.CurrentMainBaseURL != "https://elitea-gateway" {
		t.Fatalf("CurrentMainBaseURL = %q", config.CurrentMainBaseURL)
	}
}

func TestSelfCallOriginRefusesCleartextOffTheLoopback(t *testing.T) {
	// http is accepted only where nothing leaves the machine. The request
	// carries an actor bearer token.
	for name, value := range map[string]string{
		"a service name":  "http://elitea-main:8080",
		"a routable host": "http://10.1.2.3:8080",
		"a public name":   "http://elitea.example.com",
	} {
		t.Run(name, func(t *testing.T) {
			environment := agentDispatchEnvironment()
			environment["ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL"] = value
			if _, err := ConfigFromEnv(mapLookup(environment)); err == nil {
				t.Fatalf("cleartext to %q was accepted", value)
			}
		})
	}

	for name, value := range map[string]string{
		"IPv4 loopback": "http://127.0.0.1:8080",
		"IPv6 loopback": "http://[::1]:8080",
		"localhost":     "http://localhost:8080",
	} {
		t.Run(name, func(t *testing.T) {
			environment := agentDispatchEnvironment()
			environment["ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL"] = value
			if _, err := ConfigFromEnv(mapLookup(environment)); err != nil {
				t.Fatalf("the loopback self-call %q was refused: %v", value, err)
			}
		})
	}
}

func TestSelfCallOriginRefusesAnUnusableListenAddress(t *testing.T) {
	// The derivation needs a port. A listener this process cannot parse is a
	// boot failure, not a self-call to a guessed port.
	for name, value := range map[string]string{
		"no port":      "0.0.0.0",
		"not a number": ":http",
		"out of range": ":70000",
	} {
		t.Run(name, func(t *testing.T) {
			environment := agentDispatchEnvironment()
			environment["ELITEA_HTTP_ADDRESS"] = value
			if _, err := ConfigFromEnv(mapLookup(environment)); err == nil {
				t.Fatalf("ELITEA_HTTP_ADDRESS %q was accepted", value)
			}
		})
	}
}
