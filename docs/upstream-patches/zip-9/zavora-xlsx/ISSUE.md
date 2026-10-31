# Upstream submission: zavora-xlsx

Target repository: https://github.com/zavora-ai/zavora-xlsx

Two separate items go to this repository. Submit the `zip` 9 upgrade as a pull
request, with `PR.md` and the patch file. Submit the test-directory defect below
as an issue, or fold it into the same pull request.

---

## Title

Five test suites fail on a clean checkout, because no test creates `output/` or `target/`

## Body

Ten tests in four suites write to a relative `output/` path. Nothing creates
that directory, so each one fails on a clean checkout:

```
Io(NotFound)
```

| Suite | Failing tests |
| --- | --- |
| `tests/format_variants_tests.rs` | 4 |
| `tests/named_ranges_crud_tests.rs` | 2 |
| `tests/streaming_perf_tests.rs` | 3 |
| `tests/theme_color_tests.rs` | 1 |

`tests/demo_integration.rs` already shows the correct pattern. Line 6 creates
the directory first, and its 45 tests pass:

```rust
std::fs::create_dir_all(&d).unwrap();
```

The other four suites do not call it.

### Why a clean checkout matters

A developer who runs the suite after a fresh clone sees ten failures that have
no connection to their change. A `cargo test` run in CI passes only when an
earlier run left `output/` behind. The result depends on build order.

### Correction

Call `std::fs::create_dir_all("output")` at the start of each test that writes
there, or add one shared helper that each of the four suites calls. The
`demo_integration.rs` pattern is the model.

A `tempfile::TempDir` is better still. It gives each test its own directory and
removes the shared-state coupling between suites.

### A second instance: relative `target/` paths

`tests/read_sheet_protection_tests.rs` has four tests that write to a relative
`target/` path, and `examples/read_sheet_protection.rs` does the same. They pass
only because cargo creates `target/` inside the crate directory by default.
When `CARGO_TARGET_DIR` points outside the crate, which is a common CI
configuration, that directory does not exist. The four tests then fail with:

```
Io(NotFound)
```

A relative `target/` path is fragile in the same way as `output/`. The
`CARGO_TARGET_DIR` setting exposes it. The correction is the same: call
`std::fs::create_dir_all("target")` before the first write, or use a
`tempfile::TempDir`.

### Reproduction

```bash
curl -sL https://static.crates.io/crates/zavora-xlsx/zavora-xlsx-0.1.2.crate -o z.crate
mkdir -p /tmp/z && tar xzf z.crate -C /tmp/z
cd /tmp/z/zavora-xlsx-0.1.2 && cargo test --no-fail-fast
```

Expect ten failures with `Io(NotFound)`.

To see the `target/` failures, set the target directory outside the crate:

```bash
CARGO_TARGET_DIR=/tmp/xt cargo test --test read_sheet_protection_tests
```

Expect four failures with `Io(NotFound)`.
