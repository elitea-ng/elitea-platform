//! Upgrade isolation (ADR-0032 verification item 4): bumping a zavora crate
//! changes no signature outside this crate, so no vendor type may appear in
//! anything public, and nothing else in the repository may name a zavora
//! crate. Source-level checks, like the other `libs/rust` guard tests.
#![allow(clippy::pedantic)]

use std::fs;
use std::path::{Path, PathBuf};

const VENDOR: &[&str] = &[
    "zavora",
    "Zavora",
    "zip::",
    "ZipArchive",
    "ZipWriter",
    "quick_xml",
    "fontdb",
    "resvg",
    "tiny_skia",
];

fn crate_dir() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
}

fn sources() -> Vec<(String, String)> {
    let mut out = Vec::new();
    for e in fs::read_dir(crate_dir().join("src")).unwrap() {
        let p = e.unwrap().path();
        if p.extension().is_some_and(|x| x == "rs") {
            out.push((
                p.file_name().unwrap().to_string_lossy().into_owned(),
                fs::read_to_string(&p).unwrap(),
            ));
        }
    }
    out
}

/// The text of every `pub` declaration (visibility `pub`, not `pub(crate)`),
/// from the keyword to the end of its signature: the first `{`, `;` or, for a
/// field, `,` at the end of a line.
fn public_declarations(src: &str) -> Vec<String> {
    let lines: Vec<&str> = src.lines().collect();
    let mut out = Vec::new();
    let mut i = 0;
    while i < lines.len() {
        let t = lines[i].trim_start();
        if t.starts_with("pub ") && !t.starts_with("pub(") && !t.starts_with("pub(crate)") {
            let mut decl = String::new();
            loop {
                let l = lines[i].trim();
                decl.push_str(l);
                decl.push(' ');
                if l.ends_with('{')
                    || l.ends_with(';')
                    || l.ends_with(',')
                    || l.ends_with('}')
                    || l.contains('{')
                {
                    break;
                }
                i += 1;
                if i >= lines.len() {
                    break;
                }
            }
            out.push(decl);
        }
        i += 1;
    }
    out
}

#[test]
fn no_vendor_type_in_a_public_signature() {
    let mut checked = 0;
    for (file, src) in sources() {
        for decl in public_declarations(&src) {
            checked += 1;
            for v in VENDOR {
                assert!(
                    !decl.contains(v),
                    "{file}: public item mentions {v}: {decl}"
                );
            }
        }
    }
    assert!(
        checked > 100,
        "the scan found only {checked} public declarations: it is checking nothing"
    );
}

#[test]
fn the_scan_catches_a_leak() {
    // Canary for the check above: a leaking signature would be found.
    let leaked = public_declarations("pub fn open(p: zavora_slide::Presentation) -> Deck {\n}\n");
    assert!(leaked.iter().any(|d| d.contains("zavora")));
    let multiline =
        public_declarations("pub fn open(\n    p: zip::ZipArchive<R>,\n) -> Deck {\n}\n");
    assert!(multiline.iter().any(|d| d.contains("zip::")));
}

#[test]
fn error_and_limit_enums_carry_no_vendor_error() {
    let src = fs::read_to_string(crate_dir().join("src/error.rs")).unwrap();
    for v in VENDOR {
        assert!(!src.contains(v), "error.rs mentions {v}");
    }
}

#[test]
fn only_this_crate_depends_on_the_zavora_crates() {
    let root = crate_dir().join("../../..");
    let mut manifests: Vec<PathBuf> = Vec::new();
    for dir in ["libs/rust", "services", "apps", "desktop"] {
        collect(&root.join(dir), &mut manifests, 0);
    }
    assert!(
        manifests.len() > 10,
        "found {} manifests: the walk is broken",
        manifests.len()
    );
    for m in manifests {
        let text = fs::read_to_string(&m).unwrap();
        let own = m.starts_with(crate_dir());
        if !own && !text.contains("name = \"elitea-office-core-fuzz\"") {
            assert!(
                !text.contains("zavora"),
                "{} names a zavora crate",
                m.display()
            );
        }
    }
}

fn collect(dir: &Path, out: &mut Vec<PathBuf>, depth: usize) {
    let Ok(rd) = fs::read_dir(dir) else { return };
    for e in rd.flatten() {
        let p = e.path();
        let name = e.file_name().to_string_lossy().into_owned();
        if p.is_dir() {
            if depth < 4 && !matches!(name.as_str(), "target" | "node_modules" | ".git" | "vendor")
            {
                collect(&p, out, depth + 1);
            }
        } else if name == "Cargo.toml" {
            out.push(p);
        }
    }
}

#[test]
fn the_three_libraries_are_pinned_exactly_with_no_features() {
    let toml: String = fs::read_to_string(crate_dir().join("Cargo.toml"))
        .unwrap()
        .lines()
        .filter(|l| !l.trim_start().starts_with('#'))
        .collect::<Vec<_>>()
        .join("\n");
    for (name, version) in [
        ("zavora-slide", "0.1.1"),
        ("zavora-docx", "0.1.4"),
        ("zavora-xlsx", "0.1.2"),
    ] {
        let line = toml
            .lines()
            .find(|l| l.starts_with(name))
            .unwrap_or_else(|| panic!("{name} missing"));
        assert!(
            line.contains(&format!("\"={version}\"")),
            "{name} must be pinned exactly: {line}"
        );
        assert!(
            !line.contains("features = ["),
            "{name} must enable no features: {line}"
        );
    }
    assert!(
        toml.lines()
            .any(|l| l.starts_with("zavora-xlsx") && l.contains("default-features = false"))
    );
    for banned in [
        "cffi",
        "mmap",
        "crypto",
        "async-tokio",
        "\"ods\"",
        "\"xls\"",
        "xlsb",
    ] {
        assert!(!toml.contains(banned), "feature {banned} must stay off");
    }
    assert!(
        toml.contains("workspace = true"),
        "lints must come from the workspace (unsafe_code = forbid)"
    );
}

#[test]
fn unsafe_code_is_forbidden() {
    let ws = fs::read_to_string(crate_dir().join("../Cargo.toml")).unwrap();
    assert!(ws.contains("unsafe_code = \"forbid\""));
    for (file, src) in sources() {
        for kw in ["unsafe {", "unsafe fn", "unsafe impl", "unsafe trait"] {
            assert!(!src.contains(kw), "{file} contains `{kw}`");
        }
    }
}
