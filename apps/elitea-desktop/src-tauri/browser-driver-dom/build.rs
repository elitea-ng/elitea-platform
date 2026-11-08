//! Compiles the driver's page script, `js/driver.ts`, into
//! `$OUT_DIR/driver.js`, which `src/lib.rs` embeds as `SCRIPT`.
//!
//! The compiler is the web app's pinned TypeScript
//! (`apps/elitea-web/node_modules/.bin/tsc`, from `npm ci` in
//! `apps/elitea-web`), or the one `ELITEA_TSC` names. A type error in
//! `driver.ts` fails the build: there is no committed JavaScript to drift.

use std::env;
use std::path::{Path, PathBuf};
use std::process::Command;

fn main() {
    let manifest_dir = PathBuf::from(env_var("CARGO_MANIFEST_DIR"));
    let out_dir = PathBuf::from(env_var("OUT_DIR"));
    let js_dir = manifest_dir.join("js");
    let tsconfig = js_dir.join("tsconfig.json");

    println!(
        "cargo:rerun-if-changed={}",
        js_dir.join("driver.ts").display()
    );
    println!("cargo:rerun-if-changed={}", tsconfig.display());
    println!("cargo:rerun-if-env-changed=ELITEA_TSC");

    let tsc = tsc_path(&manifest_dir);
    if !tsc.is_file() {
        panic!(
            "the TypeScript compiler is not at {}: run `npm ci` in apps/elitea-web, \
             or set ELITEA_TSC to a tsc binary",
            tsc.display()
        );
    }

    let output = match Command::new(&tsc)
        .arg("-p")
        .arg(&tsconfig)
        .arg("--outDir")
        .arg(&out_dir)
        .output()
    {
        Ok(output) => output,
        Err(err) => panic!(
            "could not run {}: {err}. Run `npm ci` in apps/elitea-web, or set ELITEA_TSC \
             to a tsc binary",
            tsc.display()
        ),
    };
    if !output.status.success() {
        panic!(
            "{} -p {} failed ({}): fix js/driver.ts. If tsc itself is broken, run `npm ci` \
             in apps/elitea-web, or set ELITEA_TSC.\n--- stdout ---\n{}\n--- stderr ---\n{}",
            tsc.display(),
            tsconfig.display(),
            output.status,
            String::from_utf8_lossy(&output.stdout),
            String::from_utf8_lossy(&output.stderr),
        );
    }

    let script = out_dir.join("driver.js");
    if !script.is_file() {
        panic!(
            "{} succeeded but wrote no {}",
            tsc.display(),
            script.display()
        );
    }
}

/// `ELITEA_TSC`, or the web app's tsc at the repository root (four levels
/// above this crate: browser-driver-dom, src-tauri, elitea-desktop, apps).
fn tsc_path(manifest_dir: &Path) -> PathBuf {
    if let Some(tsc) = env::var_os("ELITEA_TSC").filter(|value| !value.is_empty()) {
        return PathBuf::from(tsc);
    }
    let Some(repo_root) = manifest_dir.ancestors().nth(4) else {
        panic!(
            "{} has no repository root four levels up; set ELITEA_TSC to a tsc binary",
            manifest_dir.display()
        );
    };
    let bin = if cfg!(windows) { "tsc.cmd" } else { "tsc" };
    repo_root
        .join("apps")
        .join("elitea-web")
        .join("node_modules")
        .join(".bin")
        .join(bin)
}

fn env_var(name: &str) -> String {
    match env::var(name) {
        Ok(value) => value,
        Err(err) => panic!("{name} is not set by cargo: {err}"),
    }
}
