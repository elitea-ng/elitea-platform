// Package nativeclient is the black-box conformance suite for the ADR-0025
// native client contract. It runs against a live deployment (the standalone
// stack in CI) and imports nothing from the services it tests.
//
// The suite itself is behind the `conformance` build tag, so a plain
// `go test ./...` (task test, the ci-go Test job) compiles none of it and
// records no skip:
//
//	go test -tags conformance -count=1 -v ./...
//
// deploy/scripts/native-conformance.sh owns the stack lifecycle and the
// environment the suite reads (ELITEA_CONFORMANCE_*; see conformance_test.go).
package nativeclient
