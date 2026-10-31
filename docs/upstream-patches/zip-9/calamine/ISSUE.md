# Upstream submission: calamine

Target repository: https://github.com/tafia/calamine

Submit this as an issue. It is not a `zip` problem. calamine 0.37.0 already
moved to `zip` 9.

---

## Title

36 doc tests fail from the published crate, because the archive ships no `tests/`

## Body

`Cargo.toml` sets `exclude = ["tests/**/*"]`, so the published `.crate` archive
holds no `tests/` directory. Many doc tests open a fixture from that directory:

```rust
/// # let path = format!("{}/tests/issue3.xlsm", env!("CARGO_MANIFEST_DIR"));
/// let mut workbook: Xlsx<_> = open_workbook(path).unwrap();
```

The `unwrap()` then panics:

```
thread 'main' panicked at src/lib.rs:8:49:
called `Result::unwrap()` on an `Err` value:
  Io(Os { code: 2, kind: NotFound, message: "No such file or directory" })
```

CI does not see this. CI runs from the git checkout, where `tests/` exists. A
user who vendors the published archive, or who runs `cargo test --doc` on an
unpacked crate, sees 36 failures.

### Reproduction

```bash
curl -sL https://static.crates.io/crates/calamine/calamine-0.36.1.crate -o c.crate
mkdir -p /tmp/c && tar xzf c.crate -C /tmp/c
cd /tmp/c/calamine-0.36.1 && cargo test --doc
```

Result on 0.36.1: `test result: FAILED. 29 passed; 36 failed`.

### Two possible corrections

1. Mark each fixture-dependent doc test `no_run`. The example still compiles and
   still documents the API. This changes no published behaviour.
2. Add the small fixtures the doc tests need to `include`, and keep the large
   corpus excluded.

Option 1 costs least and keeps the archive small.

### Scope

This affects every published release, including 0.37.0. It is independent of the
`zip` 8 to 9 upgrade.
