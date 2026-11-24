//! Credential grants (#3006): a rule the person wrote hands one named
//! program its stored login inside the sandbox.
//!
//! A grant is a [`CredentialProfile`]: which program, where its login
//! comes from, how the command gets it, and the hosts it may reach. The
//! command still runs sandboxed: its network is limited to the hosts
//! ([`crate::egress_proxy`]), the person's credential files stay hidden,
//! and only that one command's environment carries the secret.
//!
//! | Profile | Program | Source | Handed over as | Default hosts |
//! |---|---|---|---|---|
//! | `gh` | `gh` | the host runs `gh auth token --hostname github.com` itself, outside the sandbox, with a fixed argv and a scrubbed environment (gh reads its own keychain entry or `hosts.yml`) | `GH_TOKEN`; `GH_CONFIG_DIR` an empty host-owned read-only directory (no aliases, extensions or `http_unix_socket` of the person's); no update notifier, no prompts | `api.github.com`, `github.com`, `uploads.github.com` |
//!
//! A profile is never granted to the subcommands that print the secret or
//! run other code (`gh auth`, `gh alias`, `gh config`, `gh extension`),
//! whatever the rule says. The secret is cached in memory for
//! [`TOKEN_TTL`], never written to disk, never logged ([`Secret`]'s
//! `Debug` is `[REDACTED]`), and removed from the command's output as it
//! streams ([`Redactor`]).
//!
//! Who may grant: [`crate::approvals::RulesEngine::command_grant`].

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use serde::Serialize;
use tokio::io::AsyncReadExt;

use crate::error::{ErrorCode, ToolError, ToolResult};

/// How long a fetched login is reused before it is asked for again.
pub const TOKEN_TTL: Duration = Duration::from_mins(5);

/// How long the host waits for the program to print its login.
pub const FETCH_TIMEOUT: Duration = Duration::from_secs(15);

/// The most a login fetch may print: a token is far shorter.
const MAX_TOKEN_OUTPUT: usize = 8 * 1024;

/// What replaces a secret in a command's output.
pub const REDACTED: &str = "[REDACTED]";

/// One built-in credential profile.
#[derive(Debug)]
pub struct CredentialProfile {
    /// The name a rule's `credential` gives.
    pub id: &'static str,
    /// The program it is for: a rule's first pattern word must be exactly
    /// this bare name.
    pub program: &'static str,
    /// For the person (the rules panel names it).
    pub label: &'static str,
    /// The arguments the host runs the program with to read the login.
    pub token_args: &'static [&'static str],
    /// The variable the command gets the login in.
    pub env: &'static str,
    /// The variable pointed at an empty host-owned directory, so the
    /// person's own configuration of the program is never read.
    pub config_dir_env: Option<&'static str>,
    /// More variables set for the command.
    pub fixed_env: &'static [(&'static str, &'static str)],
    /// The hosts when the rule names none.
    pub default_hosts: &'static [&'static str],
    /// First words after the program that never get the login.
    pub never: &'static [&'static str],
}

/// The GitHub CLI.
pub const GH: CredentialProfile = CredentialProfile {
    id: "gh",
    program: "gh",
    label: "GitHub CLI (gh)",
    token_args: &["auth", "token", "--hostname", "github.com"],
    env: "GH_TOKEN",
    config_dir_env: Some("GH_CONFIG_DIR"),
    fixed_env: &[
        ("GH_NO_UPDATE_NOTIFIER", "1"),
        ("GH_NO_EXTENSION_UPDATE_NOTIFIER", "1"),
        ("GH_PROMPT_DISABLED", "1"),
        ("GH_SPINNER_DISABLED", "1"),
    ],
    default_hosts: &["api.github.com", "github.com", "uploads.github.com"],
    never: &["auth", "alias", "config", "extension", "extensions", "ext"],
};

/// Every built-in profile.
pub const PROFILES: &[&CredentialProfile] = &[&GH];

/// The profile named `id`.
#[must_use]
pub fn profile(id: &str) -> Option<&'static CredentialProfile> {
    PROFILES.iter().copied().find(|profile| profile.id == id)
}

impl CredentialProfile {
    /// Whether the command `argv` (its program first) may get the login:
    /// its first word after the program is a subcommand, not a flag and
    /// not one of [`Self::never`].
    #[must_use]
    pub fn admits(&self, argv: &[String]) -> bool {
        argv.get(1).is_some_and(|first| {
            !first.starts_with('-')
                && !self
                    .never
                    .iter()
                    .any(|never| first.eq_ignore_ascii_case(never))
        })
    }
}

/// What a rule grants one command, as the rules engine resolved it.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct CommandGrant {
    /// The rule that grants it (`user#3`).
    pub rule: String,
    /// The credential profile, when the rule hands over a login.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub credential: Option<String>,
    /// The hosts the command may reach (never empty).
    pub hosts: Vec<String>,
}

impl CommandGrant {
    /// The profile it hands over.
    #[must_use]
    pub fn profile(&self) -> Option<&'static CredentialProfile> {
        self.credential.as_deref().and_then(profile)
    }

    /// For a person and the model: `gh login, network limited to a, b`.
    #[must_use]
    pub fn describe(&self) -> String {
        let hosts = self.hosts.join(", ");
        match self.profile() {
            Some(profile) => format!("the {} login, network limited to {hosts}", profile.program),
            None => format!("network limited to {hosts}"),
        }
    }
}

/// A secret value: its `Debug` never shows it, and it has no `Display` or
/// `Serialize`.
#[derive(Clone, Eq, PartialEq)]
pub struct Secret(Arc<str>);

impl Secret {
    /// The value, for the one place it goes (a child's environment).
    #[must_use]
    pub fn expose(&self) -> &str {
        &self.0
    }

    /// A secret from a literal, for the crate's tests.
    #[cfg(all(test, unix))]
    pub(crate) fn for_test(value: &str) -> Self {
        Self(Arc::from(value))
    }
}

impl std::fmt::Debug for Secret {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(REDACTED)
    }
}

/// Fetched logins, by (profile, program file), with when they were read.
type Logins = HashMap<(String, PathBuf), (Secret, Instant)>;

/// Logins fetched in this session, by profile and program file.
#[derive(Clone, Default)]
pub struct CredentialCache(Arc<Mutex<Logins>>);

impl std::fmt::Debug for CredentialCache {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str("CredentialCache")
    }
}

/// Settings compare equal whatever their caches hold.
impl PartialEq for CredentialCache {
    fn eq(&self, _other: &Self) -> bool {
        true
    }
}

impl Eq for CredentialCache {}

/// Whether `value` looks like a login a CLI prints (no whitespace or
/// control characters, a sane length).
fn plausible(value: &str) -> bool {
    (8..=4096).contains(&value.len())
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"_-.:+/=".contains(&byte))
}

impl CredentialCache {
    /// The login of `profile` as `program` (the absolute file the command
    /// runs) prints it, from the cache when fresh. `env` is the
    /// environment the host runs it with (nothing else is passed).
    ///
    /// # Errors
    ///
    /// [`ErrorCode::Denied`] when the program cannot print a login (not
    /// signed in, timed out, unexpected output): nothing runs.
    pub async fn fetch(
        &self,
        profile: &CredentialProfile,
        program: &Path,
        env: &[(String, String)],
    ) -> ToolResult<Secret> {
        let key = (profile.id.to_owned(), program.to_path_buf());
        if let Ok(cache) = self.0.lock()
            && let Some((secret, at)) = cache.get(&key)
            && at.elapsed() < TOKEN_TTL
        {
            return Ok(secret.clone());
        }
        let refused = |why: &str| {
            ToolError::new(
                ErrorCode::Denied,
                format!(
                    "the rule hands this command the {} login, but {why}; sign in with `{} auth login` in a terminal, then run it again (nothing ran)",
                    profile.program, profile.program
                ),
            )
        };
        let mut child = tokio::process::Command::new(program)
            .args(profile.token_args)
            .env_clear()
            .envs(env.iter().map(|(name, value)| (name, value)))
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .kill_on_drop(true)
            .spawn()
            .map_err(|_| refused(&format!("`{}` could not be started", profile.program)))?;
        let mut stdout = child
            .stdout
            .take()
            .ok_or_else(|| refused("its output could not be read"))?;
        let read = async {
            let mut bytes = Vec::new();
            let mut chunk = [0u8; 1024];
            loop {
                let count = stdout.read(&mut chunk).await?;
                if count == 0 {
                    break;
                }
                bytes.extend_from_slice(&chunk[..count]);
                if bytes.len() > MAX_TOKEN_OUTPUT {
                    // Far more than a login: stop it rather than wait on
                    // a pipe nobody reads.
                    drop(stdout);
                    let _ = child.kill().await;
                    return Ok::<_, std::io::Error>((None, bytes));
                }
            }
            let status = child.wait().await?;
            Ok((Some(status), bytes))
        };
        let Ok(Ok((status, bytes))) = tokio::time::timeout(FETCH_TIMEOUT, read).await else {
            return Err(refused("it did not print a login in time"));
        };
        let Some(status) = status else {
            return Err(refused("it printed more than a login"));
        };
        let value = String::from_utf8(bytes).unwrap_or_default();
        let value = value.trim();
        if !status.success() || !plausible(value) {
            return Err(refused("it is not signed in"));
        }
        let secret = Secret(Arc::from(value));
        if let Ok(mut cache) = self.0.lock() {
            cache.insert(key, (secret.clone(), Instant::now()));
        }
        Ok(secret)
    }
}

/// The empty directory a profile's [`CredentialProfile::config_dir_env`]
/// points at: `<base>/<profile>`, created (owner-only) and emptied by the
/// host. `base` must be outside every writable root, so no command can
/// plant a configuration there.
///
/// # Errors
///
/// When it cannot be created, emptied or resolved.
pub fn empty_config_dir(base: &Path, profile: &CredentialProfile) -> ToolResult<PathBuf> {
    let dir = base.join(profile.id);
    let io = |error: &std::io::Error| ToolError::io("cannot prepare the grant's directory", error);
    if std::fs::read_dir(&dir).is_ok_and(|mut entries| entries.next().is_some()) {
        std::fs::remove_dir_all(&dir).map_err(|error| io(&error))?;
    }
    std::fs::create_dir_all(&dir).map_err(|error| io(&error))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o700))
            .map_err(|error| io(&error))?;
    }
    crate::sys::canonicalize(&dir).map_err(|error| io(&error))
}

/// Removes secrets from a stream as it is read: each chunk goes in, what
/// cannot be part of a secret comes out, a secret becomes [`REDACTED`].
/// The last `longest − 1` bytes wait for the next chunk, so a secret split
/// across reads is still found, and a cut of the kept output (head and
/// tail) can never leave a part of one. At the end, a trailing part of a
/// secret (4 bytes or more) is redacted too.
#[derive(Default)]
pub struct Redactor {
    secrets: Vec<Vec<u8>>,
    pending: Vec<u8>,
}

impl std::fmt::Debug for Redactor {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // Never the secrets, nor the bytes waiting (they may hold a part).
        formatter
            .debug_struct("Redactor")
            .field("secrets", &self.secrets.len())
            .finish_non_exhaustive()
    }
}

impl Redactor {
    /// A redactor for `secrets` (empty ones are ignored).
    #[must_use]
    pub fn new(secrets: &[Secret]) -> Self {
        Self {
            secrets: secrets
                .iter()
                .map(|secret| secret.expose().as_bytes().to_vec())
                .filter(|secret| !secret.is_empty())
                .collect(),
            pending: Vec::new(),
        }
    }

    fn longest(&self) -> usize {
        self.secrets.iter().map(Vec::len).max().unwrap_or(0)
    }

    /// Redact every whole secret in `pending` into `out`, leaving the
    /// bytes after the last one in `pending`.
    fn replace_whole(&mut self, out: &mut Vec<u8>) {
        loop {
            let found = self
                .secrets
                .iter()
                .filter_map(|secret| {
                    self.pending
                        .windows(secret.len())
                        .position(|window| window == secret.as_slice())
                        .map(|at| (at, secret.len()))
                })
                .min();
            let Some((at, len)) = found else {
                return;
            };
            out.extend_from_slice(&self.pending[..at]);
            out.extend_from_slice(REDACTED.as_bytes());
            self.pending.drain(..at + len);
        }
    }

    /// The bytes of `chunk` that are safe to keep now.
    pub fn push(&mut self, chunk: &[u8]) -> Vec<u8> {
        if self.secrets.is_empty() {
            return chunk.to_vec();
        }
        self.pending.extend_from_slice(chunk);
        let mut out = Vec::with_capacity(self.pending.len());
        self.replace_whole(&mut out);
        let keep = self.longest().saturating_sub(1).min(self.pending.len());
        let emit = self.pending.len() - keep;
        out.extend(self.pending.drain(..emit));
        out
    }

    /// What is left at the end of the stream.
    pub fn finish(&mut self) -> Vec<u8> {
        let mut out = Vec::new();
        self.replace_whole(&mut out);
        let mut rest = std::mem::take(&mut self.pending);
        let partial = self
            .secrets
            .iter()
            .filter_map(|secret| {
                (4..secret.len())
                    .rev()
                    .find(|len| rest.ends_with(&secret[..*len]))
            })
            .max();
        if let Some(len) = partial {
            rest.truncate(rest.len() - len);
            rest.extend_from_slice(REDACTED.as_bytes());
        }
        out.extend(rest);
        out
    }
}

#[cfg(test)]
mod tests {
    use super::{CommandGrant, GH, Redactor, Secret, profile};

    fn secret(value: &str) -> Secret {
        Secret(std::sync::Arc::from(value))
    }

    fn redact(secrets: &[&str], chunks: &[&str]) -> String {
        let secrets: Vec<Secret> = secrets.iter().map(|value| secret(value)).collect();
        let mut redactor = Redactor::new(&secrets);
        let mut out = Vec::new();
        for chunk in chunks {
            out.extend(redactor.push(chunk.as_bytes()));
        }
        out.extend(redactor.finish());
        String::from_utf8(out).expect("utf-8")
    }

    #[test]
    fn redacts_whole_and_split_secrets() {
        let token = "gho_abcdefghijklmnop";
        assert_eq!(
            redact(&[token], &["x gho_abcdefghijklmnop y"]),
            "x [REDACTED] y"
        );
        assert_eq!(
            redact(
                &[token],
                &["a gho_abc", "defghij", "klmnop b gho_abcdefghijklmnop"]
            ),
            "a [REDACTED] b [REDACTED]"
        );
        assert_eq!(redact(&[token], &["no secret here"]), "no secret here");
        assert_eq!(
            redact(&[token], &["ends with gho_abcd"]),
            "ends with [REDACTED]"
        );
        assert_eq!(
            redact(&[], &["gho_abcdefghijklmnop"]),
            "gho_abcdefghijklmnop"
        );
        for split in 1..token.len() {
            let text = format!("<{token}>");
            let (left, right) = text.split_at(split);
            assert_eq!(redact(&[token], &[left, right]), "<[REDACTED]>", "{split}");
        }
    }

    #[test]
    fn secrets_never_show_in_debug() {
        let value = secret("gho_supersecretvalue");
        assert_eq!(format!("{value:?}"), "[REDACTED]");
        let redactor = Redactor::new(&[value]);
        assert!(!format!("{redactor:?}").contains("supersecret"));
    }

    #[test]
    fn the_gh_profile() {
        assert_eq!(profile("gh").map(|profile| profile.program), Some("gh"));
        assert!(profile("npm").is_none());
        let argv = |line: &str| shlex::split(line).expect("argv");
        assert!(GH.admits(&argv("gh pr list")));
        assert!(GH.admits(&argv("gh api repos/o/r")));
        for refused in [
            "gh auth token",
            "gh AUTH status -t",
            "gh alias set x y",
            "gh config get x",
            "gh extension exec x",
            "gh ext list",
            "gh --version",
            "gh",
        ] {
            assert!(!GH.admits(&argv(refused)), "{refused}");
        }
        let grant = CommandGrant {
            rule: "user#1".to_owned(),
            credential: Some("gh".to_owned()),
            hosts: vec!["api.github.com".to_owned()],
        };
        assert_eq!(
            grant.describe(),
            "the gh login, network limited to api.github.com"
        );
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn fetches_caches_and_refuses() {
        use std::os::unix::fs::PermissionsExt as _;
        let dir = tempfile::tempdir().expect("dir");
        let write = |name: &str, body: &str| {
            let path = dir.path().join(name);
            std::fs::write(&path, format!("#!/bin/sh\n{body}\n")).expect("write");
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).expect("chmod");
            path
        };
        let counter = dir.path().join("count");
        let good = write(
            "gh",
            &format!(
                "[ \"$1 $2 $3 $4\" = 'auth token --hostname github.com' ] || exit 3\necho x >> '{}'\necho gho_fetchedtoken123",
                counter.display()
            ),
        );
        let cache = super::CredentialCache::default();
        let first = cache.fetch(&GH, &good, &[]).await.expect("token");
        assert_eq!(first.expose(), "gho_fetchedtoken123");
        let again = cache.fetch(&GH, &good, &[]).await.expect("cached");
        assert_eq!(again, first);
        assert_eq!(
            std::fs::read_to_string(&counter)
                .expect("count")
                .lines()
                .count(),
            1
        );

        let signed_out = write("gh-out", "echo 'no oauth token' >&2; exit 1");
        let error = cache
            .fetch(&GH, &signed_out, &[])
            .await
            .expect_err("refused");
        assert!(
            error.message().contains("gh auth login"),
            "{}",
            error.message()
        );
        let odd = write("gh-odd", "echo 'two words'");
        assert!(cache.fetch(&GH, &odd, &[]).await.is_err());
        let missing = dir.path().join("absent");
        assert!(cache.fetch(&GH, &missing, &[]).await.is_err());
        // A program that prints far more than a login (and would block on
        // a full pipe) is stopped at once, not waited for.
        let chatty = write("gh-chatty", "yes gho_banner | head -c 1000000; sleep 30");
        let started = std::time::Instant::now();
        let error = cache.fetch(&GH, &chatty, &[]).await.expect_err("refused");
        assert!(
            error.message().contains("more than a login"),
            "{}",
            error.message()
        );
        assert!(started.elapsed() < std::time::Duration::from_secs(5));
    }
}
