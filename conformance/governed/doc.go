// Package governed is the governed-profile conformance suite (ADR-0039 P6b,
// issue #1124). It runs against the standalone stack with the governed
// overlay (deploy/docker-compose.governed.yml) and imports nothing from the
// services it tests.
//
// Three parts:
//
//   - The end-to-end cases GOV-001 to GOV-010 and the rollback test, behind
//     the `conformance` build tag, so a plain `go test ./...` compiles none of
//     them and records no skip. deploy/scripts/governed-conformance.sh owns the
//     stack, its phases and the environment the cases read
//     (ELITEA_GOVERNED_*; see harness_test.go).
//   - The secret canary scanner (package canary, cmd/canary-scan). Every
//     credential the suite creates carries a unique canary value; after the
//     run the scanner reads the NATS tap, the database dumps, the container
//     logs (which hold the OTLP debug export) and the audit outbox, and any
//     hit fails the run. Its negative self-test runs in `go test ./...`.
//   - The flag-off golden-vector pin (package golden), also untagged.
//
// EVIDENCE.md maps every acceptance line of #1124 and of the C-1009
// requirement to a case id or a unit test, with its status.
package governed
