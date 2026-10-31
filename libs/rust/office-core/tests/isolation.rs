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

/// The dependency entries of a manifest: the lines inside `[dependencies]`,
/// `[dev-dependencies]`, `[build-dependencies]`, their `target.*` and
/// `workspace.*` forms, and a `[dependencies.<name>]` subtable. Line comments
/// are removed first.
///
/// The header of a dependency table is collected with the lines inside it,
/// because a subtable carries the crate name in the header and nowhere else:
/// `[dependencies.zavora-xlsx]` holds only `version = "=0.1.2"` in its body.
/// A plain header, like `[dependencies]` or `[target.'cfg(unix)'.dependencies]`,
/// names no crate, so it can match no crate name.
///
/// `[patch.crates-io]` is not a dependency table. A patch redirects a crate
/// that something else already requires, and `libs/rust/Cargo.toml` and
/// `services/elitea-worker-rust/Cargo.toml` carry zavora patch entries on
/// purpose (vendor/README.md). Its header is therefore not collected either.
/// A comment is not a dependency.
fn dependency_lines(manifest: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut inside = false;
    for raw in manifest.lines() {
        let line = strip_comment(raw);
        if line.starts_with('[') {
            let header = line
                .trim_start_matches('[')
                .split(']')
                .next()
                .unwrap_or_default();
            inside = header.split('.').any(|seg| {
                matches!(
                    seg.trim(),
                    "dependencies" | "dev-dependencies" | "build-dependencies"
                )
            });
            if inside {
                out.push(line.to_owned());
            }
        } else if inside && !line.is_empty() {
            out.push(line.to_owned());
        }
    }
    out
}

/// `line` without its trailing TOML comment: a `#` at the start of the line,
/// or one after whitespace. A `#` inside a value keeps its line.
fn strip_comment(line: &str) -> &str {
    let t = line.trim();
    if t.starts_with('#') {
        return "";
    }
    match t.find(" #") {
        Some(i) => t[..i].trim_end(),
        None => t,
    }
}

#[test]
fn only_this_crate_depends_on_the_zavora_crates() {
    // Canonical paths. `Path::starts_with` compares components and does not
    // resolve `..`, so an unnormalised root made every manifest start with
    // this crate's own components and the assertion below never ran. That is
    // the false-green class of issue #423.
    let own = fs::canonicalize(crate_dir()).unwrap();
    let root = fs::canonicalize(crate_dir().join("../../..")).unwrap();
    // Walk the whole repository. A fixed list of roots was the guard's real
    // coverage boundary: it held `desktop`, which does not exist, and it
    // missed `scripts/runtime/fixtures/code-data-processing`. `collect` skips
    // `target`, `node_modules`, `.git` and `vendor` by name.
    let mut manifests: Vec<PathBuf> = Vec::new();
    collect(&root, &mut manifests, 0);
    // Non-vacuity floor. The walk found 39 manifests when this was written.
    assert!(
        manifests.len() > 36,
        "found {} manifests: the walk is broken",
        manifests.len()
    );
    let mut foreign = 0;
    let mut entries = 0;
    for m in manifests {
        // `collect` skips `vendor/`: the vendored crates ARE the zavora
        // crates, and they name each other legitimately. `own` covers
        // `office-core/fuzz`, which is part of this crate.
        if m.starts_with(&own) {
            continue;
        }
        foreign += 1;
        let text = fs::read_to_string(&m).unwrap();
        for line in dependency_lines(&text) {
            entries += 1;
            assert!(
                !line.contains("zavora"),
                "{} depends on a zavora crate: {line}",
                m.display()
            );
        }
    }
    // Non-vacuity floors, close to the real counts: the scan read 37 other
    // manifests and 684 dependency entries when this was written. A walk that
    // stopped finding most of the tree must fail, not report a pass
    // (scripts/ci/check-rust-toolchain.sh, point (d)).
    assert!(
        foreign > 34 && entries > 600,
        "the scan read {foreign} other manifests and {entries} dependency entries: it is checking nothing"
    );
}

#[test]
fn the_dependency_scan_catches_a_leak() {
    // Canary for the check above. It replaces a guard that asserted nothing.
    let leak = "[package]\nname = \"x\"\n\n[dependencies]\nzavora-xlsx = \"=0.1.2\"\n";
    assert!(
        dependency_lines(leak).iter().any(|l| l.contains("zavora")),
        "a real dependency must be found"
    );
    let dev = "[dev-dependencies]\nzavora-docx = \"=0.1.4\"\n";
    assert!(dependency_lines(dev).iter().any(|l| l.contains("zavora")));
    let target = "[target.'cfg(unix)'.build-dependencies]\nzavora-slide = \"=0.1.1\"\n";
    assert!(
        dependency_lines(target)
            .iter()
            .any(|l| l.contains("zavora"))
    );
    // A subtable names the crate in the header only. A collected line must
    // contain the name. `!is_empty()` proved nothing here: the body line
    // `version = "=0.1.2"` satisfied it while the crate name escaped.
    let sub = "[dependencies.zavora-xlsx]\nversion = \"=0.1.2\"\n";
    assert!(
        dependency_lines(sub).iter().any(|l| l.contains("zavora")),
        "a subtable dependency must be found: {:?}",
        dependency_lines(sub)
    );
    let sub_dev = "[dev-dependencies.zavora-docx]\nversion = \"=0.1.4\"\n";
    assert!(
        dependency_lines(sub_dev)
            .iter()
            .any(|l| l.contains("zavora")),
        "a dev subtable dependency must be found"
    );
    let sub_target = "[target.'cfg(unix)'.dependencies.zavora-slide]\nversion = \"=0.1.1\"\n";
    assert!(
        dependency_lines(sub_target)
            .iter()
            .any(|l| l.contains("zavora")),
        "a target subtable dependency must be found"
    );

    // A plain dependency table header names no crate, so it is no false
    // positive. The header is collected; it carries no crate name.
    for plain in [
        "[dependencies]\nserde = \"1\"\n",
        "[dev-dependencies]\nserde = \"1\"\n",
        "[target.'cfg(unix)'.dependencies]\nserde = \"1\"\n",
    ] {
        assert!(
            !dependency_lines(plain).iter().any(|l| l.contains("zavora")),
            "a plain header must name no crate: {:?}",
            dependency_lines(plain)
        );
    }

    // The three things that are not a dependency.
    let patch = "[patch.crates-io]\nzavora-slide = { path = \"vendor/zavora-slide\" }\n";
    assert!(dependency_lines(patch).is_empty(), "a patch is not a dep");
    let patch_sub = "[patch.crates-io.zavora-slide]\npath = \"vendor/zavora-slide\"\n";
    assert!(
        dependency_lines(patch_sub).is_empty(),
        "a patch subtable is not a dep"
    );
    let comment = "[dependencies]\n# zavora-xlsx moved to elitea-office-core\nserde = \"1\"\n";
    assert!(
        !dependency_lines(comment)
            .iter()
            .any(|l| l.contains("zavora")),
        "a comment is not a dep"
    );
    let trailing = "[dependencies]\nserde = \"1\" # not zavora\n";
    assert!(
        !dependency_lines(trailing)
            .iter()
            .any(|l| l.contains("zavora")),
        "a trailing comment is not a dep"
    );
}

fn collect(dir: &Path, out: &mut Vec<PathBuf>, depth: usize) {
    let Ok(rd) = fs::read_dir(dir) else { return };
    for e in rd.flatten() {
        let p = e.path();
        let name = e.file_name().to_string_lossy().into_owned();
        if p.is_dir() {
            // The walk starts at the repository root, so the limit counts from
            // there. The deepest manifest is
            // `libs/rust/local-tools/tests/fixtures/projects/cargo/Cargo.toml`,
            // 7 directories down; `depth < 8` reaches it and keeps one level
            // of headroom. The old limit of 4 missed it.
            if depth < 8 && !matches!(name.as_str(), "target" | "node_modules" | ".git" | "vendor")
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

#[test]
fn every_locked_zavora_package_is_at_the_pinned_version() {
    // Exact pins on the three top-level crates do not pin their sub-crates:
    // the lock does. This fails when a sub-crate moves (a `cargo update`, or
    // a Dependabot grouped bump that slipped past the ignore rule) without
    // the reviewed PR that reruns the hostile corpus.
    let lock = fs::read_to_string(crate_dir().join("../Cargo.lock")).unwrap();
    let mut found: Vec<(String, String)> = Vec::new();
    let mut name = None;
    for line in lock.lines() {
        if let Some(n) = line.strip_prefix("name = \"") {
            name = Some(n.trim_end_matches('"').to_owned());
        } else if let (Some(v), Some(n)) = (line.strip_prefix("version = \""), name.take())
            && n.starts_with("zavora-")
        {
            found.push((n, v.trim_end_matches('"').to_owned()));
        }
    }
    found.sort();
    let expected: Vec<(String, String)> = [
        ("zavora-docx", "0.1.4"),
        ("zavora-docx-html", "0.1.4"),
        ("zavora-docx-layout", "0.1.4"),
        ("zavora-docx-opc", "0.1.4"),
        ("zavora-docx-oxml", "0.1.4"),
        ("zavora-docx-pdf", "0.1.4"),
        ("zavora-slide", "0.1.1"),
        ("zavora-slide-layout", "0.1.1"),
        ("zavora-slide-opc", "0.1.1"),
        ("zavora-slide-oxml", "0.1.1"),
        ("zavora-slide-pdf", "0.1.1"),
        ("zavora-slide-render", "0.1.1"),
        ("zavora-slide-xlsx", "0.1.1"),
        ("zavora-xlsx", "0.1.2"),
    ]
    .iter()
    .map(|(n, v)| ((*n).to_owned(), (*v).to_owned()))
    .collect();
    assert_eq!(
        found, expected,
        "a zavora-* package moved in libs/rust/Cargo.lock"
    );
}

#[test]
fn dependabot_ignores_zavora_for_libs_rust() {
    let cfg = fs::read_to_string(crate_dir().join("../../../.github/dependabot.yml")).unwrap();
    let at = cfg
        .find("package-ecosystem: cargo")
        .expect("a cargo ecosystem");
    let block_end = cfg[at..]
        .find("\n  - package-ecosystem")
        .map_or(cfg.len(), |i| at + i);
    let block = &cfg[at..block_end];
    assert!(
        block.contains("- /libs/rust\n"),
        "the cargo entry must be the one that scans libs/rust"
    );
    assert!(
        block.contains("ignore:") && block.contains("dependency-name: \"zavora-*\""),
        "the libs/rust cargo entry must ignore zavora-*:\n{block}"
    );
}
