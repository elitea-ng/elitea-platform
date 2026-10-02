# Authenticated credential browser acceptance, 2026-09-14

A fresh headed Chrome session creates a temporary PAT through Personal Tokens.
An independent Python MCP client creates GitHub configuration 16 and toolkit 51.
The toolkit stores the configuration title with `private: false`.
The credential contains a synthetic token for an isolated TLS fixture, not GitHub.
The fixture validates the authorization header and returns only a generation marker.

The browser runs `get_issues` from Toolkit Test and displays `RUST_SECRET_BEFORE`.
Execution `b8b6dba4edc3d2da2086960c110e0afe` succeeds in the native Rust GitHub family.
The client rotates the configuration through `put_configurations_configuration`.
The browser reloads the same toolkit and runs the same tool without replacing its credential reference.
It displays `RUST_SECRET_AFTER`.
Execution `19308f0ecbfc5239cd10b78d116c3e02` succeeds.
The fixture records two authenticated requests and two completed responses.

Neither the internal MCP responses nor the rendered toolkit page contains the synthetic credential value.
This observation complements the existing PostgreSQL sealing test; it does not inspect encrypted storage during this browser run.
Main retains configuration sealing and claim materialization ownership.
Rust `families/github/config.rs` parses the materialized credential.
Rust `families/github/client.rs` supplies the authorization header and validates the provider response.
The source mappings in `chat-credential-toolkit-creation.md` remain the business references.
No product source, schema, or deployment changes are needed for this proof.

Toolkit deletion, configuration deletion, and PAT revocation each return HTTP 204.
The temporary fixture process stops without restarting the OAuth emulator.
Evidence is `elitea-secret-browser-corrected.log`, `elitea-secret-browser-before.png`, and `elitea-secret-browser-after.png`.
The browser script is `elitea-secret-browser.py`; the temporary provider is `elitea-secret-fixture.py`.

The first fixture omits required timestamps and label/assignee arrays.
Execution `3c414e89b718c1ccbdbc682126573890` receives authenticated data but rejects that incomplete response.
That browser assertion fails, and all first-attempt fixtures are removed.
The corrected fixture supplies the required GitHub fields. No platform repair is inferred from this test-fixture correction.
