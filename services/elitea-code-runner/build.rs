//! Record the compiler that builds this crate as the native bundle's `rust_revision`.
//!
//! `elitea-code-rust` compiles user Cargo bundles inside the `rust-runtime`
//! image, the same pinned `rust:<version>-trixie` image this crate is built
//! in, so the compiler compiling this crate IS the compiler that compiles the
//! bundles. Reading it here replaces a hardcoded release that raising the pin
//! would have left stale (every bundle mislabelled, the release audit red).
use std::{env, process::Command};

fn main() {
    let rustc = env::var("RUSTC").unwrap_or_else(|_| "rustc".to_owned());
    let output = Command::new(&rustc)
        .arg("-V")
        .output()
        .unwrap_or_else(|error| panic!("cannot run `{rustc} -V`: {error}"));
    assert!(output.status.success(), "`{rustc} -V` failed");
    let line = String::from_utf8(output.stdout).expect("`rustc -V` printed non-UTF-8");
    let version = release(&line)
        .unwrap_or_else(|| panic!("cannot read an exact X.Y.Z release from `{}`", line.trim()));
    println!("cargo:rustc-env=ELITEA_RUSTC_RELEASE={version}");
    println!("cargo:rerun-if-env-changed=RUSTC");
    println!("cargo:rerun-if-changed=build.rs");
}

/// `rustc X.Y.Z (<hash> <date>)` -> `X.Y.Z`; anything else -> None.
fn release(line: &str) -> Option<&str> {
    line.strip_prefix("rustc ")
        .and_then(|rest| rest.split_whitespace().next())
        .filter(|version| {
            let parts: Vec<_> = version.split('.').collect();
            parts.len() == 3
                && parts
                    .iter()
                    .all(|p| !p.is_empty() && p.bytes().all(|b| b.is_ascii_digit()))
        })
}

#[cfg(test)]
mod tests {
    use super::release;

    #[test]
    fn reads_only_an_exact_release() {
        // Built at run time so this file carries no `rustc X.Y.Z` literal:
        // check-rust-toolchain.sh (e) holds every such literal to the pin,
        // and these are parser inputs, not release claims.
        let line = |rest: &str| format!("rustc {rest}");
        assert_eq!(
            release(&line("4.5.6 (8bab26f4f 2026-07-14)\n")),
            Some("4.5.6")
        );
        assert_eq!(
            release(&line("10.20.30 (abc 2026-09-01) (Homebrew)\n")),
            Some("10.20.30")
        );
        for refused in [
            String::new(),
            "cargo 4.5.6 (c980f4866 2026-06-30)".to_owned(),
            line("4.5 (x)"),
            line("4.5.6-nightly (x)"),
            line("4.5.6.7 (x)"),
            line(" (x)"),
        ] {
            assert_eq!(release(&refused), None, "{refused:?}");
        }
    }
}
