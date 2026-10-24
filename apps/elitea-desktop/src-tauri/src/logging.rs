//! Diagnostics: one log file per install, for support (README, "Logs").
//!
//! Rust and the webview both write through `tauri-plugin-log` (the webview
//! through `plugin:log|log`, its only log permission). Targets: stdout, and a
//! size-capped file in the OS log directory — on macOS
//! `~/Library/Logs/ai.elitea.desktop/elitea.log`. Info and above in release
//! builds, debug in debug builds.
//!
//! Every line passes [`scrub`] on the way out, whoever wrote it: the webview
//! scrubs its own messages first (`shared/desktop/diagnostics.ts`), this is
//! the second net for anything that reaches the logger another way (an error
//! that quotes a URL with a `code`, a dependency's message).

use tauri_plugin_log::{RotationStrategy, Target, TargetKind};

/// The log file's name, without its extension.
pub const LOG_FILE_NAME: &str = "elitea";
/// The file is rotated past this size; one previous file is kept.
pub const MAX_FILE_BYTES: u128 = 5 * 1024 * 1024;

/// HTTP and TLS internals: chatty at debug and of no use to support.
const QUIET_MODULES: &[&str] = &[
    "hyper",
    "hyper_util",
    "reqwest",
    "rustls",
    "h2",
    "tao",
    "wry",
    "tracing",
];

/// Parameter names whose values are credentials or one-time codes.
const SECRET_KEYS: &[&str] = &[
    "access_token",
    "refresh_token",
    "id_token",
    "token",
    "code",
    "code_verifier",
    "client_secret",
    "password",
    "authorization",
];

#[must_use]
pub fn level() -> log::LevelFilter {
    if cfg!(debug_assertions) {
        log::LevelFilter::Debug
    } else {
        log::LevelFilter::Info
    }
}

pub fn plugin<R: tauri::Runtime>() -> tauri::plugin::TauriPlugin<R> {
    let mut builder = tauri_plugin_log::Builder::new()
        .clear_targets()
        .targets([
            Target::new(TargetKind::Stdout),
            Target::new(TargetKind::LogDir {
                file_name: Some(LOG_FILE_NAME.to_owned()),
            }),
        ])
        .level(level())
        .max_file_size(MAX_FILE_BYTES)
        .rotation_strategy(RotationStrategy::KeepSome(1))
        .format(|out, message, record| {
            out.finish(format_args!(
                "{} {:<5} [{}] {}",
                chrono::Local::now().format("%Y-%m-%dT%H:%M:%S%.3f%:z"),
                record.level(),
                record.target(),
                scrub(&message.to_string())
            ));
        });
    for module in QUIET_MODULES {
        builder = builder.level_for(*module, log::LevelFilter::Warn);
    }
    builder.build()
}

/// Redact what must never reach a log file: bearer tokens, `Authorization`
/// values, credential parameters (`refresh_token=…`, `"code":"…"`), and the
/// query string of any URL (sign-in codes travel there).
#[must_use]
pub fn scrub(input: &str) -> String {
    let without_queries = strip_url_queries(input);
    let without_bearer = redact_after_word(&without_queries, "bearer");
    redact_secret_values(&without_bearer)
}

fn is_value_end(c: char) -> bool {
    c.is_whitespace() || matches!(c, '"' | '\'' | ',' | ';' | '&' | '}' | ')' | ']' | '>')
}

/// `https://host/path?code=x#frag` -> `https://host/path?[redacted]`.
fn strip_url_queries(input: &str) -> String {
    let mut out = String::with_capacity(input.len());
    let mut rest = input;
    while let Some(start) = rest.find("://") {
        let (before, after) = rest.split_at(start + 3);
        out.push_str(before);
        // A URL runs to whitespace or a closing quote/bracket; `&` and `;` belong to it.
        let end = after
            .find(|c: char| c.is_whitespace() || matches!(c, '"' | '\'' | ')' | ']' | '>' | '}'))
            .unwrap_or(after.len());
        let url = &after[..end];
        match url.find(['?', '#']) {
            Some(q) => {
                out.push_str(&url[..q]);
                out.push_str("?[redacted]");
            }
            None => out.push_str(url),
        }
        rest = &after[end..];
    }
    out.push_str(rest);
    out
}

/// The token after `word ` (case-insensitive), e.g. `Bearer abc` -> `Bearer [redacted]`.
fn redact_after_word(input: &str, word: &str) -> String {
    let lower = input.to_ascii_lowercase();
    let mut out = String::with_capacity(input.len());
    let mut i = 0;
    while let Some(found) = lower[i..].find(word) {
        let at = i + found;
        let after = at + word.len();
        let boundary = at == 0 || !lower.as_bytes()[at - 1].is_ascii_alphanumeric();
        if boundary && input[after..].starts_with(' ') {
            let value_start = after + 1;
            let value_len = input[value_start..]
                .find(is_value_end)
                .unwrap_or(input.len() - value_start);
            out.push_str(&input[i..value_start]);
            out.push_str(if value_len == 0 { "" } else { "[redacted]" });
            i = value_start + value_len;
        } else {
            out.push_str(&input[i..after]);
            i = after;
        }
    }
    out.push_str(&input[i..]);
    out
}

/// `key=value`, `key: value`, `"key":"value"` for every [`SECRET_KEYS`] name.
fn redact_secret_values(input: &str) -> String {
    let lower = input.to_ascii_lowercase();
    let bytes = lower.as_bytes();
    let mut out = String::with_capacity(input.len());
    let mut i = 0;
    let mut cursor = 0;
    while cursor < input.len() {
        let hit = SECRET_KEYS.iter().find(|key| {
            lower[cursor..].starts_with(*key)
                && (cursor == 0
                    || !(bytes[cursor - 1].is_ascii_alphanumeric() || bytes[cursor - 1] == b'_'))
                && lower[cursor + key.len()..]
                    .chars()
                    .next()
                    .is_some_and(|c| !(c.is_ascii_alphanumeric() || c == '_'))
        });
        let Some(key) = hit else {
            cursor += lower[cursor..].chars().next().map_or(1, char::len_utf8);
            continue;
        };
        let mut j = cursor + key.len();
        // An optional closing quote, then `=` or `:`, then optional space and quote.
        if lower[j..].starts_with('"') || lower[j..].starts_with('\'') {
            j += 1;
        }
        if !(lower[j..].starts_with('=') || lower[j..].starts_with(':')) {
            cursor += key.len();
            continue;
        }
        j += 1;
        while lower[j..].starts_with(' ') {
            j += 1;
        }
        if lower[j..].starts_with('"') || lower[j..].starts_with('\'') {
            j += 1;
        }
        // An `Authorization` value is a scheme AND a credential: all of it goes.
        let end_of_value = |c: char| {
            if *key == "authorization" {
                matches!(c, '"' | '\'' | ',' | ';' | '\n' | '}')
            } else {
                is_value_end(c)
            }
        };
        let len = input[j..].find(end_of_value).unwrap_or(input.len() - j);
        out.push_str(&input[i..j]);
        if len > 0 {
            out.push_str("[redacted]");
        }
        i = j + len;
        cursor = i;
    }
    out.push_str(&input[i..]);
    out
}

/// The ACP agent's log file (src/acp), beside the app's own: the two
/// processes run at once, and stdout is the protocol there.
pub const ACP_LOG_FILE_NAME: &str = "elitea-acp.log";

/// Logging for `--acp`: the app's format, level and scrubbing, into
/// [`ACP_LOG_FILE_NAME`] in `dir` (created `0700`, the file `0600`), rotated
/// once past [`MAX_FILE_BYTES`] at start. Without a directory, nothing is
/// logged; never to stdout.
pub fn init_acp(dir: Option<&std::path::Path>) {
    let Some(dir) = dir else {
        return;
    };
    let Some(file) = open_acp_log(dir) else {
        return;
    };
    let logger: &'static AcpLogger = Box::leak(Box::new(AcpLogger {
        file: std::sync::Mutex::new(file),
    }));
    if log::set_logger(logger).is_ok() {
        log::set_max_level(level());
    }
}

fn open_acp_log(dir: &std::path::Path) -> Option<std::fs::File> {
    let mut builder = std::fs::DirBuilder::new();
    builder.recursive(true);
    #[cfg(unix)]
    std::os::unix::fs::DirBuilderExt::mode(&mut builder, 0o700);
    builder.create(dir).ok()?;
    let path = dir.join(ACP_LOG_FILE_NAME);
    if std::fs::symlink_metadata(&path)
        .is_ok_and(|meta| meta.is_file() && u128::from(meta.len()) > MAX_FILE_BYTES)
    {
        let _ = std::fs::rename(&path, dir.join(format!("{ACP_LOG_FILE_NAME}.1")));
    }
    let mut options = std::fs::OpenOptions::new();
    options.create(true).append(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        options.mode(0o600);
        options.custom_flags(rustix::fs::OFlags::NOFOLLOW.bits().cast_signed());
    }
    options.open(path).ok()
}

struct AcpLogger {
    file: std::sync::Mutex<std::fs::File>,
}

impl log::Log for AcpLogger {
    fn enabled(&self, metadata: &log::Metadata<'_>) -> bool {
        let quiet = QUIET_MODULES.iter().any(|module| {
            metadata.target() == *module
                || metadata
                    .target()
                    .strip_prefix(module)
                    .is_some_and(|rest| rest.starts_with("::"))
        });
        metadata.level()
            <= if quiet {
                log::Level::Warn
            } else {
                log::Level::Trace
            }
    }

    fn log(&self, record: &log::Record<'_>) {
        if !self.enabled(record.metadata()) {
            return;
        }
        let line = format!(
            "{} {:<5} [{}] {}\n",
            chrono::Local::now().format("%Y-%m-%dT%H:%M:%S%.3f%:z"),
            record.level(),
            record.target(),
            scrub(&record.args().to_string())
        );
        if let Ok(mut file) = self.file.lock() {
            let _ = std::io::Write::write_all(&mut *file, line.as_bytes());
        }
    }

    fn flush(&self) {
        if let Ok(mut file) = self.file.lock() {
            let _ = std::io::Write::flush(&mut *file);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::scrub;

    #[test]
    fn bearer_tokens_and_authorization_headers_are_redacted() {
        assert_eq!(
            scrub("Authorization: Bearer eyJabc.def"),
            "Authorization: [redacted]"
        );
        assert_eq!(
            scrub(r#"{"authorization":"Basic Zm9v","a":1}"#),
            r#"{"authorization":"[redacted]","a":1}"#
        );
        assert_eq!(
            scrub("sent bearer abc123 to it"),
            "sent bearer [redacted] to it"
        );
    }

    #[test]
    fn credential_parameters_are_redacted_in_any_notation() {
        assert_eq!(
            scrub("refresh_token=r1&x=1"),
            "refresh_token=[redacted]&x=1"
        );
        assert_eq!(
            scrub(r#"{"refresh_token":"r1","ok":true}"#),
            r#"{"refresh_token":"[redacted]","ok":true}"#
        );
        assert_eq!(scrub("code: abc"), "code: [redacted]");
        // A word that merely contains a key is left alone.
        assert_eq!(scrub("statuscode=200 encode=x"), "statuscode=200 encode=x");
    }

    #[test]
    fn url_query_strings_are_dropped() {
        assert_eq!(
            scrub("GET http://127.0.0.1:5555/callback?code=abc&state=s failed"),
            "GET http://127.0.0.1:5555/callback?[redacted] failed"
        );
        assert_eq!(
            scrub("https://x.example/a#access_token=t"),
            "https://x.example/a?[redacted]"
        );
        assert_eq!(scrub("no url here"), "no url here");
    }
}
