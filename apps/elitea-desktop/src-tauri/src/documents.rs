//! The bundled document extractor (ADR-0029 decision 4).
//!
//! `read_document` needs the PDF and Office extractor, and xberg cannot be
//! linked into this host: it turns on `serde_json/preserve_order`, which
//! decision 2 keeps out. So the extractor ships as a second binary,
//! `apps/elitea-desktop/doc-extract-sidecar`, with a lockfile of its own, and
//! the Tauri bundle places it next to the app's executable
//! (`tauri.sidecar.conf.json`, `bundle.externalBin`). The local tools run it
//! under the OS sandbox: read-only, no network, the session's deny list, an
//! empty environment, a wall-clock limit.

use std::path::{Path, PathBuf};

use elitea_local_tools::document::Documents;

/// The sidecar's file name next to the host executable (Tauri strips the
/// target triple from an `externalBin` when it bundles it).
pub const SIDECAR_NAME: &str = "elitea-doc-extract-sidecar";

/// Where `read_document` gets PDF and Office text for this run: the
/// sidecar bundled beside the executable, or, when there is none (a
/// `cargo run` build without the bundle step), text files only.
pub fn bundled() -> Documents {
    std::env::current_exe()
        .ok()
        .and_then(|exe| beside(&exe))
        .map_or(Documents::InProcess, Documents::Sidecar)
}

/// The sidecar in the directory of `exe`, when it is a regular file.
fn beside(exe: &Path) -> Option<PathBuf> {
    let candidate = exe.parent()?.join(SIDECAR_NAME);
    candidate.is_file().then_some(candidate)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_sidecar_is_found_beside_the_executable_only() {
        let dir = tempfile::tempdir().expect("dir");
        let exe = dir.path().join("Elitea");
        assert_eq!(beside(&exe), None);
        std::fs::create_dir(dir.path().join(SIDECAR_NAME)).expect("a directory is no sidecar");
        assert_eq!(beside(&exe), None);
        std::fs::remove_dir(dir.path().join(SIDECAR_NAME)).expect("rm");
        std::fs::write(dir.path().join(SIDECAR_NAME), b"#!/bin/sh\n").expect("sidecar");
        assert_eq!(beside(&exe), Some(dir.path().join(SIDECAR_NAME)));
    }

    /// ADR-0029 decision 2: the extractor (xberg, and the TOON crate it
    /// brings) turns on `serde_json/preserve_order`; the host must never link
    /// it, which `Cargo.lock` shows whatever the features say.
    #[test]
    fn desktop_host_does_not_link_xberg() {
        let lock =
            std::fs::read_to_string(Path::new(env!("CARGO_MANIFEST_DIR")).join("Cargo.lock"))
                .expect("Cargo.lock");
        for package in ["xberg", "serde_toon_format"] {
            assert!(
                !lock.contains(&format!("name = \"{package}\"")),
                "{package} is in the desktop host's dependency graph; documents are extracted by the sidecar (apps/elitea-desktop/doc-extract-sidecar)"
            );
        }
    }

    /// The host's own configuration of the sidecar (a session opened with
    /// [`Documents::Sidecar`]) runs it confined: it cannot read the stored
    /// sign-in, write, or open a socket, whatever the document asks.
    #[cfg(target_os = "macos")]
    #[tokio::test]
    async fn sidecar_runs_read_only_without_network() {
        use std::net::TcpListener;
        use std::os::unix::fs::PermissionsExt as _;
        use std::sync::Arc;

        use elitea_local_tools::approvals::{DeferringPrompt, MemoryChoices, WorkspaceSettings};
        use elitea_local_tools::policy::LocalWorkPolicy;
        use elitea_local_tools::session::{LocalSession, SessionConfig};
        use elitea_local_tools::shell::ShellConfig;

        if !Path::new("/usr/bin/sandbox-exec").is_file() {
            return;
        }
        let dir = tempfile::tempdir().expect("dir");
        let base = std::fs::canonicalize(dir.path()).expect("canonical");
        let home = base.join("home");
        let app_data = home.join("Library/Application Support/ai.elitea.desktop");
        std::fs::create_dir_all(&app_data).expect("app data");
        std::fs::write(app_data.join("credentials.json"), "SECRET-REFRESH-TOKEN").expect("creds");
        let root = base.join("work");
        std::fs::create_dir(&root).expect("work");
        std::fs::write(root.join("contract.pdf"), b"%PDF-1.4").expect("pdf");
        let listener = TcpListener::bind("127.0.0.1:0").expect("listen");
        listener.set_nonblocking(true).expect("nonblocking");
        let port = listener.local_addr().expect("addr").port();
        let script = base.join(SIDECAR_NAME);
        std::fs::write(
            &script,
            format!(
                r#"#!/bin/sh
cat > /dev/null
read_out=$(cat '{creds}' 2>&1 | tr -d '"\n')
write_out=$(touch '{target}' 2>&1 | tr -d '"\n')
net_out=$(/usr/bin/nc -z -G 2 127.0.0.1 {port} >/dev/null 2>&1 && echo connected || echo blocked)
printf '{{"ok":true,"extractor":"fake","page_count":1,"pages":[{{"n":1,"text":"read=%s|write=%s|net=%s"}}]}}' "$read_out" "$write_out" "$net_out"
"#,
                creds = app_data.join("credentials.json").display(),
                target = root.join("written").display(),
            ),
        )
        .expect("script");
        std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755)).expect("chmod");
        let mut shell = ShellConfig::new(base.join("data/tmp"));
        shell.home = Some(home);
        let session = LocalSession::open(SessionConfig {
            root: root.clone(),
            session_id: "s".to_owned(),
            policy: LocalWorkPolicy {
                allowed: true,
                ..LocalWorkPolicy::default()
            },
            settings: WorkspaceSettings::default(),
            choices: Arc::new(MemoryChoices::default()),
            prompt: Arc::new(DeferringPrompt),
            data_dir: base.join("data"),
            shell: Some(shell),
            deny_read: Vec::new(),
            app_id: Some("ai.elitea.desktop".to_owned()),
            budgets: elitea_local_tools::budget::Budgets::default(),
            documents: Documents::Sidecar(script),
        })
        .expect("session");
        let answer = session
            .call(
                "read_document",
                "c",
                serde_json::json!({ "path": "contract.pdf" }),
            )
            .await;
        assert_eq!(answer["status"], "ok", "{answer}");
        let text = answer["text"].as_str().expect("text");
        assert!(text.contains("Operation not permitted"), "{text}");
        assert!(!text.contains("SECRET-REFRESH-TOKEN"), "{text}");
        assert!(text.contains("net=blocked"), "{text}");
        assert!(!root.join("written").exists(), "the sidecar wrote: {text}");
        assert!(
            matches!(listener.accept(), Err(ref e) if e.kind() == std::io::ErrorKind::WouldBlock),
            "the sidecar reached loopback"
        );
    }
}
