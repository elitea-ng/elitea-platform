# Runtime application version HTTP contract v1

Schema discriminator: `elitea.runtime.application-version.v1`.
Authoritative JSON Schema: `libs/jsonschema/runtime/v1/application-version.schema.json`.

This response uses the existing private HTTP/JSON content route.
No protobuf message owns this response.
This record versions its cross-language contract without adding generated protobuf bindings.
Main and Worker must use the typed envelope described here.
An editable application map does not provide envelope authority.

## Envelope

| Field | Type | Requirement |
| --- | --- | --- |
| `schema_version` | string | Exact discriminator above |
| `project_id` | integer | Claim-authorized project, from 1 through 2,147,483,647 |
| `application_id` | integer | Exact requested and resolved application, from 1 through 2,147,483,647 |
| `version_id` | integer | Exact requested and resolved saved version, from 1 through 2,147,483,647 |
| `version_details` | nonempty object | Main's claim-bound materialized application version |
| `frozen_definition_sha256` | optional string | Exactly 64 lowercase hexadecimal characters |

Unknown envelope fields are invalid.
A present null, uppercase digest, wrong-length digest, or non-hexadecimal digest is invalid.
Omission preserves the legacy envelope.
A valid digest containing only zero digits remains syntactically valid.
Its authority still comes from the authenticated claim-bound route.

## Producer digest

Only Main produces `frozen_definition_sha256`.
Main uses the exact bytes returned by `FreezeCurrentApplicationVersion` before credential redemption.
Main computes the digest before calling `MaterializeCurrentApplicationVersion`.
It does not hash the returned credential-bearing `version_details`.
It does not select a digest from stored metadata, aliases, names, or request fields.

The domain is these exact UTF-8 bytes, including the final zero byte:

```text
elitea.runtime.application-definition.v1\0
```

The SHA-256 input concatenates these fields in this order:

1. Domain bytes.
2. Claim-authorized project ID as unsigned 64-bit big-endian bytes.
3. Resolved application ID as unsigned 64-bit big-endian bytes.
4. Resolved version ID as unsigned 64-bit big-endian bytes.
5. Frozen JSON byte length as unsigned 64-bit big-endian bytes.
6. Exact frozen pre-redemption JSON bytes.

Main emits the 32 digest bytes as 64 lowercase hexadecimal characters without a prefix.
JSON whitespace and key order are part of this exact-byte identity.
No JSON canonicalization is specified or inferred.
This digest identifies a frozen definition. It is not an authorization grant or signature.
The complete HTTP response retains its existing transport content digest independently.
That content digest identifies response bytes and can change when redeemed credentials change.

## Consumer binding

Worker first verifies the existing workload channel, claim response, schema, project, application, and version.
It then stores the optional digest in a sealed transport-owned definition identity.
The identity binds the resolved application and version.
Worker never recomputes this digest from materialized application data.
Worker never obtains this identity from inner metadata, an alias, or a local label.
Identity-dependent Parallel and scoped flows must require the sealed present identity.
Omitted identities retain legacy behavior without granting those flows a fabricated identity.

## Compatibility and deployment

The added field is optional for new consumers.
The current legacy Worker rejects unknown envelope fields.
Deploy the compatible Worker before deploying this Main producer.
Deploying the new producer ahead of the legacy Worker breaks nested application loading.
No negotiated feature switch or capability discovery is introduced by this packet.

## Fixture and verification

Use `services/elitea-main/internal/infra/storage/testdata/runtime_application_definition_digest_v1.json` for the public fixture vector.
Its frozen JSON string represents exact UTF-8 bytes without a trailing newline.
The outer fixture file's whitespace does not enter the definition digest.
Focused tests must prove stable identity across credential rotation and changed identity across frozen bytes or resolved identities.
Reject invalid frozen material before credential redemption.
Preserve claim authorization, tenant binding, identity validation, cancellation, and response limits.

This private packet does not prove deployed Worker/Main compatibility or database behavior.
