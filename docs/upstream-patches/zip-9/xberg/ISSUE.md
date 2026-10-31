# Upstream submission: xberg

Target repository: https://github.com/xberg-io/xberg

Submit this as an issue, or as a pull request if you change the range yourself.

---

## Title

`calamine ^0.36.1` holds a CVE-affected `zip` 8 in the dependency graph

## Body

The `excel` feature depends on `calamine ^0.36.1`. `calamine` is a `0.x` crate,
so this range means `>=0.36.1, <0.37.0`. Cargo cannot select `calamine` 0.37.0
for any `xberg` consumer.

`calamine` 0.36.1 depends on `zip ^8.6`. That `zip` version carries four
unfixed advisories:

| Advisory | Severity | Fixed in |
| --- | --- | --- |
| CVE-2026-115905 | CRITICAL | `zip` 9.0.5 |
| CVE-2026-115907 | HIGH | `zip` 9.0.0 |
| CVE-2026-115908 | HIGH | `zip` 9.0.0 |
| CVE-2026-115909 | HIGH | `zip` 9.0.0 |

`calamine` 0.37.0 already corrects this. It depends on `zip ^9.0`. The range in
`xberg` is the only reason the vulnerable version stays in the graph.

Every consumer that enables `excel` inherits `zip` 8.6.0. A consumer cannot
correct this alone. A `[patch.crates-io]` entry on `calamine` is the only local
workaround, and each consumer must repeat it.

### Why this is low risk for this project

`xberg` 1.3.10 already depends on `zip ^9.0.2` directly. The manifest declares
it as a normal optional dependency and as a dev-dependency. The project builds
against `zip` 9 today. Only the transitive copy through `calamine` stays on
`zip` 8, so the upgrade removes a duplicate major version rather than
introducing a new one.

### Requested change

Change the `calamine` requirement to admit 0.37:

```toml
calamine = { version = "0.37", optional = true }
```

Then correct any call site that `calamine` 0.36 -> 0.37 changed.

### Verification to run

```bash
cargo update
cargo tree -i zip --features tokio-runtime,pdf,office,excel,email,html
```

Expect only `zip` 9.x in the output.

## Evidence

Read the facts from the crates.io index:

```bash
curl -s https://crates.io/api/v1/crates/xberg/1.3.10/dependencies \
  | jq -r '.dependencies[] | select(.crate_id=="calamine" or .crate_id=="zip") | "\(.crate_id) \(.req) \(.kind)"'
curl -s https://crates.io/api/v1/crates/calamine/0.36.1/dependencies \
  | jq -r '.dependencies[] | select(.crate_id=="zip") | "0.36.1 -> zip \(.req)"'
curl -s https://crates.io/api/v1/crates/calamine/0.37.0/dependencies \
  | jq -r '.dependencies[] | select(.crate_id=="zip") | "0.37.0 -> zip \(.req)"'
```
