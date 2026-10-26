# web and host_intel families (#1238)

`web_search` and `web_fetch` (`web`), `internet_host_search` and
`internet_host_lookup` (`host_intel`). Every provider call is admitted before
any provider I/O and settled after it through the gateway's metered API
(`meter.rs`). Fetched content reaches the model framed as untrusted data.

## Adding a provider for an existing capability

1. Add `adapters/<id>.rs`: `ORIGINS` (https only), `construct`, and the
   capability trait it serves. Report units the way the vendor bills them.
2. Add one `RegistryEntry` in `registry.rs`. No other file branches on a
   provider id.
3. Add fixtures under `fixtures/web/<id>/` (an ok answer, an empty one,
   `error_401`, `error_429`) and list them in `web_test_support::fixture`.
   The contract tests run every adapter against them.
4. Main: add the configuration type, a price row in
   `gateway.metered_operation_prices`, and the provider to the `web`
   settings enums. Then regenerate `rust_only_toolkit_tools.json` with
   `ELITEA_WRITE_RUST_ONLY_TOOLS=1` (`catalogue.rs`).
5. The worker's default egress allowlist comes from the registry origins,
   so it needs no change. A deployment that sets `webEgressAllowlist`
   must add the new host.

## Adding a capability

Add `capability/<kind>.rs` (schema, parse, run), add a slot to
`ProviderHandle` and a `Capability` variant, and give it a `WebToolKind`.
