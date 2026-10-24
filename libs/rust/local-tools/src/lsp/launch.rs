//! Starting one language server: under the session's sandbox, its own
//! process group, a scrubbed environment, then `initialize`.
//!
//! Language servers run the project's code (`build.rs` and proc macros,
//! tsserver plugins, `go generate`), so the request is fixed whatever the
//! policy allows commands: the workspace **read-only**, the network **off**
//! (loopback too), the session's `DenyList` and `path_deny` hidden, and one
//! writable directory, the server's own cache under the session's
//! temporary directory (rust-analyzer's target directory, Go's build
//! cache, `TMPDIR`). A server starts only where that sandbox is fully
//! enforced ([`admissible`]): never under `partial` or `none`.

use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant};

use serde_json::{Value, json};
use tokio::io::AsyncReadExt as _;

use super::client::{Connection, path_uri};
use super::registry::Language;
use crate::policy::SandboxMode;
use crate::process::TERM_GRACE;
use crate::sandbox::{self, Enforcement, Prepared};
use crate::session::LocalSession;
use crate::shell;

/// How long `initialize` may take.
pub const INITIALIZE_TIMEOUT: Duration = Duration::from_secs(20);

/// The variables a server gets set to its own cache, whatever the
/// passthrough held.
const OWN_VARIABLES: &[&str] = &[
    "TMPDIR",
    "XDG_CACHE_HOME",
    "CARGO_TARGET_DIR",
    "CARGO_NET_OFFLINE",
    "GOCACHE",
    "GOPROXY",
    "GOTOOLCHAIN",
];

/// Whether a server may run under `prepared`: only a sandbox that is
/// fully enforced and hides everything the session denies (a container
/// is its own boundary).
#[must_use]
pub fn admissible(prepared: &Prepared) -> bool {
    matches!(
        prepared.enforcement,
        Enforcement::Full | Enforcement::Container
    ) && prepared.hides_denied
}

/// The environment a server gets: `base` (the shell's scrubbed
/// passthrough, never the host's environment) with its caches, temporary
/// directory and the build tools' offline switches pointing at `cache`.
#[must_use]
pub fn server_environment(base: Vec<(String, String)>, cache: &Path) -> Vec<(String, String)> {
    let mut env: Vec<(String, String)> = base
        .into_iter()
        .filter(|(name, _)| !OWN_VARIABLES.contains(&name.as_str()))
        .collect();
    let at = |name: &str| cache.join(name).display().to_string();
    env.extend(
        [
            ("TMPDIR", at("tmp")),
            ("XDG_CACHE_HOME", at("xdg")),
            ("CARGO_TARGET_DIR", at("target")),
            ("CARGO_NET_OFFLINE", "true".to_owned()),
            ("GOCACHE", at("go-build")),
            // The module cache stays where it is, read-only: an empty one
            // with the network off would leave every import unresolved.
            ("GOPROXY", "off".to_owned()),
            ("GOTOOLCHAIN", "local".to_owned()),
        ]
        .map(|(name, value)| (name.to_owned(), value)),
    );
    env
}

/// The settings a server is handed (`initializationOptions`, and its
/// `workspace/configuration` answers by section).
#[must_use]
pub fn settings(language: Language, cache: &Path) -> Value {
    match language {
        Language::Rust => json!({
            "rust-analyzer": {
                "cargo": { "targetDir": cache.join("target").display().to_string() },
                "checkOnSave": true,
            }
        }),
        _ => json!({}),
    }
}

/// A started server's process and connection. Dropping it stops the
/// process group (`SIGTERM`, then `SIGKILL` after [`TERM_GRACE`]).
pub struct Process {
    pub connection: Arc<Connection>,
    group: Option<rustix::process::Pid>,
    exited: Arc<AtomicBool>,
}

impl Process {
    /// Whether the process has ended or its output closed.
    pub fn exited(&self) -> bool {
        self.exited.load(Ordering::SeqCst) || self.connection.closed()
    }

    fn signal(&self, signal: rustix::process::Signal) {
        if let Some(group) = self.group
            && !self.exited.load(Ordering::SeqCst)
        {
            let _ = rustix::process::kill_process_group(group, signal);
        }
    }
}

impl Drop for Process {
    fn drop(&mut self) {
        if self.exited.load(Ordering::SeqCst) {
            return;
        }
        self.signal(rustix::process::Signal::TERM);
        let (group, exited) = (self.group, Arc::clone(&self.exited));
        let _ = std::thread::Builder::new()
            .name("elitea-lsp-stop".to_owned())
            .spawn(move || {
                let deadline = Instant::now() + TERM_GRACE;
                while !exited.load(Ordering::SeqCst) && Instant::now() < deadline {
                    std::thread::sleep(Duration::from_millis(20));
                }
                if let Some(group) = group
                    && !exited.load(Ordering::SeqCst)
                {
                    let _ =
                        rustix::process::kill_process_group(group, rustix::process::Signal::KILL);
                }
            });
    }
}

/// Everything decided before a server starts.
pub struct Launch {
    language: Language,
    prepared: Prepared,
    env: Vec<(String, String)>,
    root: PathBuf,
    cache: PathBuf,
}

impl Launch {
    /// Plan `program args…` for `language` in `session`.
    ///
    /// # Errors
    ///
    /// Why it cannot start, as the person reads it: a program inside the
    /// workspace or a denied folder, a cache that cannot be made, or a
    /// sandbox that is not fully enforced.
    pub fn plan(
        session: &LocalSession,
        language: Language,
        program: &Path,
        args: &[String],
    ) -> Result<Self, String> {
        let workspace = session.workspace();
        let resolved = std::fs::canonicalize(program)
            .map_err(|_| format!("`{}` does not exist", program.display()))?;
        if resolved.starts_with(workspace.root()) {
            return Err("a language server's program cannot be inside the workspace".to_owned());
        }
        if session.deny_list().covers(&resolved) || session.deny_list().covers(program) {
            return Err("a language server's program cannot be in a denied folder".to_owned());
        }
        let shell = session.shell_config();
        let cache = shell.temp_dir.join("lsp").join(language.as_str());
        for sub in ["", "tmp", "xdg", "target", "go-build"] {
            use std::os::unix::fs::DirBuilderExt as _;
            std::fs::DirBuilder::new()
                .recursive(true)
                .mode(0o700)
                .create(cache.join(sub))
                .map_err(|_| "cannot create the language server's cache".to_owned())?;
        }
        let cache = std::fs::canonicalize(&cache)
            .map_err(|_| "cannot resolve the language server's cache".to_owned())?;
        // The session's request (its deny list and `path_deny`), with the
        // workspace taken out of what may be written: only the cache is.
        let mut request = session
            .sandbox_request(SandboxMode::WorkspaceWrite, false)
            .map_err(|error| error.message().to_owned())?;
        request.writable_roots = vec![cache.clone()];
        request.protected = Vec::new();
        request.git_roots = Vec::new();
        request.network = false;
        request.loopback = false;
        request.allow_listen = false;
        let mut command = vec![program.display().to_string()];
        command.extend(args.iter().cloned());
        let prepared = sandbox::prepare(&request, &command, &shell.sandbox)
            .map_err(|error| format!("{}; diagnostics off", error.message()))?;
        if !admissible(&prepared) {
            return Err(format!(
                "the sandbox here is {} (not full), so language servers do not run; diagnostics off",
                prepared.enforcement.as_str()
            ));
        }
        let env = server_environment(shell::process_environment(shell, false), &cache);
        Ok(Self {
            language,
            prepared,
            env,
            root: workspace.root().to_path_buf(),
            cache,
        })
    }

    /// Start it and run `initialize`.
    ///
    /// # Errors
    ///
    /// It could not start, or did not initialize in time.
    pub async fn spawn(self) -> Result<Process, String> {
        let mut command = tokio::process::Command::new(&self.prepared.program);
        command
            .args(&self.prepared.args)
            .env_clear()
            .envs(self.env.iter().map(|(name, value)| (name, value)))
            .current_dir(&self.root)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .process_group(0)
            .kill_on_drop(true);
        let mut child = command
            .spawn()
            .map_err(|error| format!("the language server could not start: {}", error.kind()))?;
        let group = child
            .id()
            .and_then(|pid| i32::try_from(pid).ok())
            .and_then(rustix::process::Pid::from_raw);
        let (Some(stdin), Some(stdout), Some(mut stderr)) =
            (child.stdin.take(), child.stdout.take(), child.stderr.take())
        else {
            return Err("the language server has no pipes".to_owned());
        };
        // Its stderr is drained and dropped: never logged, never shown to
        // the model.
        tokio::spawn(async move {
            let mut buffer = [0u8; 4096];
            while let Ok(read) = stderr.read(&mut buffer).await {
                if read == 0 {
                    break;
                }
            }
        });
        let exited = Arc::new(AtomicBool::new(false));
        {
            let exited = Arc::clone(&exited);
            tokio::spawn(async move {
                let _ = child.wait().await;
                exited.store(true, Ordering::SeqCst);
            });
        }
        let root = self.root.display().to_string();
        let root_uri = path_uri(&root);
        let name = self
            .root
            .file_name()
            .map_or_else(|| root.clone(), |name| name.to_string_lossy().into_owned());
        let folder = json!({ "uri": root_uri, "name": name });
        let settings = settings(self.language, &self.cache);
        let initialization = settings
            .get("rust-analyzer")
            .cloned()
            .unwrap_or(Value::Null);
        let connection = Connection::new(Box::new(stdin), settings, folder.clone());
        connection.spawn_reader(stdout);
        let process = Process {
            connection,
            group,
            exited,
        };
        let params = json!({
            "processId": std::process::id(),
            "clientInfo": { "name": "elitea-desktop" },
            "rootPath": root,
            "rootUri": root_uri,
            "workspaceFolders": [folder],
            "initializationOptions": initialization,
            "capabilities": {
                "textDocument": {
                    "synchronization": { "dynamicRegistration": false, "didSave": true },
                    "publishDiagnostics": { "relatedInformation": false, "versionSupport": true }
                },
                "workspace": { "configuration": true, "workspaceFolders": true },
                "window": { "workDoneProgress": false }
            }
        });
        process
            .connection
            .request("initialize", params, INITIALIZE_TIMEOUT)
            .await
            .map_err(|why| format!("the language server did not initialize: {why}"))?;
        process
            .connection
            .notify("initialized", json!({}))
            .await
            .map_err(|why| format!("the language server did not initialize: {why}"))?;
        Ok(process)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn env_is_the_scrubbed_base_with_the_servers_own_caches() {
        let base = vec![
            ("PATH".to_owned(), "/usr/bin:/bin".to_owned()),
            ("TMPDIR".to_owned(), "/tmp/session".to_owned()),
            ("GOCACHE".to_owned(), "/shared/go".to_owned()),
        ];
        let env = server_environment(base, Path::new("/cache"));
        let get = |name: &str| {
            env.iter()
                .find(|(key, _)| key == name)
                .map(|(_, value)| value.as_str())
        };
        assert!(std::env::var_os("CARGO_MANIFEST_DIR").is_some());
        assert_eq!(get("CARGO_MANIFEST_DIR"), None, "never the host's own");
        assert_eq!(get("PATH"), Some("/usr/bin:/bin"));
        assert_eq!(get("TMPDIR"), Some("/cache/tmp"));
        assert_eq!(get("GOCACHE"), Some("/cache/go-build"));
        assert_eq!(get("CARGO_TARGET_DIR"), Some("/cache/target"));
        assert_eq!(get("GOPROXY"), Some("off"));
        assert_eq!(env.iter().filter(|(key, _)| key == "TMPDIR").count(), 1);
    }

    #[test]
    fn only_a_full_sandbox_admits_a_server() {
        let prepared = |enforcement, hides_denied| Prepared {
            program: PathBuf::from("/usr/bin/sandbox-exec"),
            args: Vec::new(),
            enforcement,
            note: None,
            hides_denied,
        };
        assert!(admissible(&prepared(Enforcement::Full, true)));
        assert!(admissible(&prepared(Enforcement::Container, true)));
        assert!(!admissible(&prepared(Enforcement::Full, false)));
        assert!(!admissible(&prepared(Enforcement::Partial, true)));
        assert!(!admissible(&prepared(Enforcement::None, true)));
    }
}
