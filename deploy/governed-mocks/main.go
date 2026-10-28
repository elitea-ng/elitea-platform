// Command governed-mocks is the external-provider test double of the
// governed-profile conformance stack (deploy/docker-compose.governed.yml,
// ADR-0039 P6b, issue #1124).
//
// It plays every third party the governed cases call, on ONE TLS listener
// routed by Host, so the compose network can give it the providers' own host
// names as aliases:
//
//	jira.governed.test           a Jira-like REST target (static secret)
//	sharepoint.governed.test     the SharePoint site a target binding names
//	graph.microsoft.com          the Graph calls the SharePoint family makes
//	login.microsoftonline.com    an Entra token endpoint: on-behalf-of and
//	                             client credentials, OIDC metadata and JWKS
//	entra.governed.test          the inbound MCP token issuer (JWKS)
//
// A plain admin listener serves what a test on the host reads: the request
// journal, the inbound token minting and the health check. The journal never
// holds a credential. It records the SHA-256 of the credential each request
// presented, so a test proves WHICH credential reached the provider by
// hashing the canary it seeded, and the canary scan of the stack's logs and
// dumps never finds it here.
//
// Subcommands:
//
//	serve                      run the listeners
//	healthcheck                exit 0 when the admin listener answers
//	mint-tls -out DIR -hosts   a CA and a leaf for the hosts (build time)
//	mint-material -out DIR     the stack's run-time test material (one-shot)
//
// Standard library only. Python is being retired, so the new doubles are Go.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "healthcheck":
		err = healthcheck(os.Args[2:])
	case "mint-tls":
		err = mintTLS(os.Args[2:])
	case "mint-material":
		err = mintMaterial(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "governed-mocks:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: governed-mocks serve|healthcheck|mint-tls|mint-material [flags]")
	os.Exit(2)
}
