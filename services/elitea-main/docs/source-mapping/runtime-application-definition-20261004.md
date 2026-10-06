# Frozen application definition identity producer

Date: 2026-10-04.
Status: tested private producer packet. No product integration or deployment.

The inspected worktree HEAD is `ad4da99ff2dcf63b9c92c76e1fadb837c550153a`.
The packet preserves unrelated concurrent changes.
All edits occur in private source copies.

## Contract and source map

| Behavior | Inspected source | Private change |
| --- | --- | --- |
| Claim-bound saved Agent lookup | `internal/infra/storage/runtime_application_version.go:RuntimeApplicationVersionService.Resolve`, line 136 | Preserve claim authorization, claim-derived project and actor, and exact resolved application/version checks |
| Pre-redemption freeze | `runtime_application_version.go`, line 209 | Compute identity after freeze and before the existing materializer call |
| Credential redemption boundary | `runtime_application_version.go`, line 225 | Keep redeemed response bytes outside definition identity |
| Saved configuration reference mode | `internal/application/configurations/toolkit_settings.go`, lines 38-43, 456 | Reference mode retains sealed references; claim mode owns redemption |
| Production composition | `internal/runtimecomposition/composition.go`, lines 631 and 1332 | Preserve existing parent freezer and claim-bound materializer owners |
| Response envelope | `runtime_application_version.go:RuntimeApplicationVersionContext`, line 60 | Add optional `frozen_definition_sha256` with legacy omission serialization |
| Existing transport integrity | `internal/infra/storage/content_server.go:PostApplicationVersion`, lines 585, 630, 642 | Preserve response size limit, private cache headers, and response Content-Digest |
| Cross-language schema | Existing route uses HTTP/JSON; no protobuf message owns its response | Add `libs/jsonschema/runtime/v1/application-version.schema.json` and linked versioned `libs/proto/elitea/runtime/v1/runtime_application_version_v1.md` before producer implementation |
| Definition identity producer | No existing producer field | Add `runtime_application_definition.go:runtimeApplicationDefinitionSHA256` |
| Strict Worker consumer | Worker `src/transport/runtime_context.rs:ApplicationVersionResponse` rejects unknown fields | Separate Worker owner adds optional strict digest parsing and sealed identity |
| Route envelope verification | Existing `runtime_application_version_test.go` expects five fields | Expect six fields and lowercase 64-hex identity from the producer |
| Public digest fixture | No existing fixture | Add `testdata/runtime_application_definition_digest_v1.json` |

The digest domain is `elitea.runtime.application-definition.v1` followed by one zero byte.
Framing contains project, application, version, and JSON byte length as four unsigned 64-bit big-endian values.
Exact frozen pre-redemption JSON bytes follow those values.
The producer emits lowercase hexadecimal SHA-256 without a prefix.
Stored metadata, names, aliases, and redeemed secrets never supply the emitted digest.
Metadata within the frozen bytes remains ordinary definition data.

The public vector uses project 17, application 31, version 41, and 65 exact JSON bytes.
Its digest is `31e7aff6b702375b6eaca24b124cf465ff9b9c587756b655e53b8256342395d5`.
Worker's owner independently reproduces the vector and preserves the byte-identical fixture.

## Verification

Private focused Go package tests pass.
They execute nine new top-level tests and seven existing nested-version top-level tests.
Seventeen named table subtests also pass.
No matched test skips or fails.

New tests cover the agreed public vector and identity changes across exact bytes and resolved identities.
They prove stable identity while only redeemed credentials change.
An in-place materializer mutation proves that hashing occurs before redemption.
Tests also cover editable metadata, invalid frozen material before redemption, cancellation, legacy omission, and strict schema validation.
Schema tests reject null, uppercase, wrong length, non-hexadecimal, prefixed, numeric, newline, and unknown-field values.
They accept legacy omission and syntactically valid zero digests.

Existing route tests retain claim refusal, identity refusal, capability refusal, response limits, route absence, and composition checks.
Go vet and Go formatting pass for the private package and changed Go files.
The tests use service doubles and an in-process HTTP router.
They do not establish real database, mTLS deployment, Worker recovery, or cross-process compatibility.

Commands use the existing Go dependencies and these environment settings:

```sh
GOWORK=off
GOPROXY=off
GOSUMDB=off
GOTOOLCHAIN=local
GOFLAGS=-mod=readonly
GOCACHE=/private/tmp/elitea-main-frozen-definition-feature-20261004/go-cache
go test -p 1 -count=1 -run 'TestRuntimeApplicationDefinition|TestNestedApplicationVersion' -v ./internal/infra/storage
go vet -p 1 ./internal/infra/storage
gofmt -l internal/infra/storage/runtime_application_version.go internal/infra/storage/runtime_application_version_test.go internal/infra/storage/runtime_application_definition.go internal/infra/storage/runtime_application_definition_test.go
```

The module declares Go 1.25.8. The installed test compiler is Go 1.26.5.
No module, dependency, or generated binding changes occur.
These private paths document the test environment only.

## Compatibility and remaining work

New Worker accepts legacy omission.
Present null or malformed identity fails closed.
Legacy Worker rejects the added envelope field.
Deploy the compatible Worker before this Main producer.
The packet introduces no feature switch or negotiation protocol.

The definition digest is neither an authorization grant nor a durable effect receipt.
Parallel and scoped adoption must use the sealed claim-bound Worker identity.
Their runtime integration and failure-mode acceptance remain separate work.
No HTTP action transport work occurs in this producer packet.

## Implementation history

2026-10-04: inspect saved Agent materialization, freeze ownership, strict Worker response parsing, and existing schema locations.
Agree on the optional field, domain, four identity frames, and fixture with the Worker owner.
Establish the versioned HTTP/JSON contract before producer edits.
Add private producer code, source mapping, baseline/postimage copies, and focused tests.
Keep all shared product files unchanged.
