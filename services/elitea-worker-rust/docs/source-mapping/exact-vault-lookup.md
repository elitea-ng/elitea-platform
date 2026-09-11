# Exact vault lookup for Rust chat admission

## Source mapping

Current `projects/centry/pylon_main/plugins/secrets/api/v2/secret.py` reads a named secret from regular and hidden dictionaries.
New `services/elitea-main/internal/api/v2/secrets/handler.go::ResolveSecretValue` serves exact named references for runtime configuration and chat defaults.
Rust receives admitted settings through the existing claim-authorized input service.
Rust does not own or migrate the application vault.

## Implementation

Existing vaults contain non-string values, including numeric model project identifiers.
Decoding every value as a string prevents otherwise valid exact lookups.
The new helper reuses the existing database lookup and Fernet decryption.
The caller decodes only the selected value as a string.
Regular secrets retain precedence over hidden secrets.
Missing values retain `ErrSecretNotFound`.
Numeric selected values fail without exposing their contents.
Reads do not change stored vault bytes or keys.

## Verification

The PostgreSQL regression fixture contains regular, hidden, numeric, and boolean values.
It verifies precedence, missing references, rejected numeric credentials, and unchanged encrypted storage.
The secrets, folders, and conversation package tests pass on 2026-09-09.
The deployed repair permits creation of the chat used for the Rust internal MCP gate.
Main's master key matches the source Centry Main key through an in-memory comparison.
No migration or key replacement is required.

Bulk vault APIs still use the existing whole-vault decoder.
This repair does not prove compatibility for their mixed-type write operations.
