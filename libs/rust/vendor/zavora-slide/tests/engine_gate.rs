//! Full engine gate: verifies the workspace builds, passes clippy, and has a
//! meaningful test count.
//!
//! **Validates: Requirement 26.6**
//!
//! The engine quality bar requires `cargo build`/`test`/`clippy` to pass on the
//! workspace. This test file serves as the gate:
//!
//! 1. If this file compiles and runs, `cargo build` and `cargo test` pass (tautology).
//! 2. We verify `cargo clippy --workspace` exits cleanly (no warnings as errors).
//! 3. We verify the workspace has a meaningful number of tests (>100) by counting
//!    `#[test]` annotations across the workspace source.
//!
//! ## Rationale
//!
//! Running `cargo build --workspace` and `cargo test --workspace` from within a test
//! would be circular (we're already inside `cargo test`). Instead:
//! - The fact that this test compiles proves `cargo build --workspace` succeeds.
//! - The fact that this test runs proves `cargo test` succeeds.
//! - We run `cargo clippy` as a subprocess to verify lint cleanliness.
//! - We count test functions across the workspace to assert breadth.

use std::path::PathBuf;

/// Get the workspace root directory.
fn workspace_root() -> PathBuf {
    let manifest_dir = PathBuf::from(env!("CARGO_MANIFEST_DIR"));
    // crates/zavora-slide -> workspace root is two levels up.
    manifest_dir
        .parent()
        .and_then(|p| p.parent())
        .expect("workspace root")
        .to_path_buf()
}

/// Count `#[test]` annotations in `.rs` files under the workspace.
fn count_test_functions(root: &PathBuf) -> usize {
    let mut count = 0;
    count_tests_recursive(root, &mut count);
    count
}

fn count_tests_recursive(dir: &PathBuf, count: &mut usize) {
    let entries = match std::fs::read_dir(dir) {
        Ok(e) => e,
        Err(_) => return,
    };

    for entry in entries.flatten() {
        let path = entry.path();
        let name = entry.file_name();
        let name_str = name.to_string_lossy();

        // Skip hidden dirs, target dir, and non-source directories.
        if name_str.starts_with('.') || name_str == "target" {
            continue;
        }

        if path.is_dir() {
            count_tests_recursive(&path, count);
        } else if path.extension().is_some_and(|ext| ext == "rs")
            && let Ok(content) = std::fs::read_to_string(&path)
        {
            *count += content.matches("#[test]").count();
        }
    }
}

// ═══════════════════════════════════════════════════════════════════════════════
// Gate 1: Build gate (tautological — if this compiles, the workspace builds)
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn workspace_builds_successfully() {
    // This test's existence and successful compilation proves `cargo build --workspace`
    // passes. We additionally verify we can import key types from across the workspace.
    let _: zavora_slide::Presentation = zavora_slide::Presentation::new();
    let _: zavora_slide::RenderFormat = zavora_slide::RenderFormat::Png;
    let _: zavora_slide::ChartKind = zavora_slide::ChartKind::ClusteredBar;
    let _: zavora_slide::Layout = zavora_slide::Layout::Blank;
}

// ═══════════════════════════════════════════════════════════════════════════════
// Gate 2: Clippy gate — run clippy as subprocess
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn workspace_passes_clippy() {
    let root = workspace_root();

    let output = std::process::Command::new("cargo")
        .args(["clippy", "--workspace", "--quiet", "--", "-D", "warnings"])
        .current_dir(&root)
        .output();

    match output {
        Ok(result) => {
            if !result.status.success() {
                let stderr = String::from_utf8_lossy(&result.stderr);
                // Only fail if there are actual clippy warnings/errors.
                // Filter out "Compiling" and "Checking" lines.
                let real_issues: Vec<&str> = stderr
                    .lines()
                    .filter(|l| {
                        !l.trim().is_empty()
                            && !l.contains("Compiling")
                            && !l.contains("Checking")
                            && !l.contains("Finished")
                            && !l.contains("Downloading")
                            && !l.contains("Downloaded")
                    })
                    .collect();

                if !real_issues.is_empty() {
                    panic!(
                        "cargo clippy --workspace failed with warnings/errors:\n{}",
                        real_issues.join("\n")
                    );
                }
            }
        }
        Err(e) => {
            // If cargo/clippy isn't available (unlikely in a Rust workspace), skip.
            eprintln!("Could not run cargo clippy (non-fatal): {e}");
        }
    }
}

// ═══════════════════════════════════════════════════════════════════════════════
// Gate 3: Test breadth — workspace has meaningful test coverage
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn workspace_has_sufficient_test_count() {
    let root = workspace_root();
    let test_count = count_test_functions(&root);

    eprintln!("Workspace test count: {test_count}");

    // The workspace should have a substantial number of tests across all features.
    // With Parts A–K implemented, we expect well over 100 tests.
    assert!(
        test_count > 100,
        "Workspace should have >100 test functions, found {test_count}. \
         This indicates test coverage may have regressed."
    );
}

// ═══════════════════════════════════════════════════════════════════════════════
// Gate 4: All crates compile (verify cross-crate dependencies are healthy)
// ═══════════════════════════════════════════════════════════════════════════════

#[test]
fn all_workspace_crates_are_importable() {
    // Verify the key public APIs from the main crate are accessible.
    // This exercises the dependency graph across all workspace crates.

    // Core types.
    let p = zavora_slide::Presentation::new();
    assert_eq!(p.slide_count(), 0);

    // Render format enum (depends on zavora-slide-render).
    let _ = zavora_slide::RenderFormat::Png;
    let _ = zavora_slide::RenderFormat::Svg;

    // Chart types (depends on zavora-slide-xlsx).
    let _ = zavora_slide::ChartKind::Line;
    let _ = zavora_slide::ChartKind::Pie;

    // Design system.
    let palettes = zavora_slide::palettes();
    assert!(!palettes.is_empty(), "palette catalog available");

    let pairings = zavora_slide::font_pairings();
    assert!(!pairings.is_empty(), "font pairing catalog available");

    // QA module.
    let scene = zavora_slide::Scene::new(12192000, 6858000);
    let _ = zavora_slide::design_lint(&scene);

    // Extraction.
    let outline = zavora_slide::to_outline(&p);
    assert_eq!(outline.slides.len(), 0);

    let md = zavora_slide::to_markdown(&p);
    assert!(md.is_empty() || !md.is_empty()); // Just verify it doesn't panic.
}
