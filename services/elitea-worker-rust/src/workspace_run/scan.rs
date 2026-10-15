//! The hydrate-time secret re-scan (ADR-0034 decision 5, defence in depth).
//!
//! The desktop scans before upload and blocks a send with a finding; the
//! worker scans the checked-out tree again before the sandbox starts. A
//! finding fails the turn. Findings carry the rule and the line only; the
//! path is never logged or audited (decision 12), and the matched text is
//! never kept.
//!
//! The rules are data ([`RULES`]): token shapes with a fixed prefix and a
//! bounded body, private-key armour, and connection strings with a password.
//! No network service is consulted.

use std::path::{Path, PathBuf};

/// The character class of a token body.
#[derive(Clone, Copy, Debug)]
enum Body {
    /// `[A-Z0-9]`
    UpperAlnum,
    /// `[A-Za-z0-9]`
    Alnum,
    /// `[A-Za-z0-9_-]`
    UrlSafe,
}

impl Body {
    const fn admits(self, byte: u8) -> bool {
        match self {
            Self::UpperAlnum => byte.is_ascii_uppercase() || byte.is_ascii_digit(),
            Self::Alnum => byte.is_ascii_alphanumeric(),
            Self::UrlSafe => byte.is_ascii_alphanumeric() || byte == b'_' || byte == b'-',
        }
    }
}

/// One token rule: `prefix` then `min..=max` body characters, not preceded
/// or followed by another body character.
#[derive(Clone, Copy, Debug)]
pub(crate) struct TokenRule {
    pub(crate) id: &'static str,
    prefix: &'static str,
    body: Body,
    min: usize,
    max: usize,
}

/// The token rules. Kept in step with the desktop's send-time scan.
pub(crate) const RULES: &[TokenRule] = &[
    TokenRule { id: "aws_access_key_id", prefix: "AKIA", body: Body::UpperAlnum, min: 16, max: 16 },
    TokenRule { id: "aws_temporary_access_key_id", prefix: "ASIA", body: Body::UpperAlnum, min: 16, max: 16 },
    TokenRule { id: "github_token", prefix: "ghp_", body: Body::Alnum, min: 36, max: 36 },
    TokenRule { id: "github_oauth_token", prefix: "gho_", body: Body::Alnum, min: 36, max: 36 },
    TokenRule { id: "github_app_token", prefix: "ghs_", body: Body::Alnum, min: 36, max: 36 },
    TokenRule { id: "github_user_token", prefix: "ghu_", body: Body::Alnum, min: 36, max: 36 },
    TokenRule { id: "github_refresh_token", prefix: "ghr_", body: Body::Alnum, min: 36, max: 76 },
    TokenRule { id: "github_fine_grained_token", prefix: "github_pat_", body: Body::UrlSafe, min: 70, max: 100 },
    TokenRule { id: "gitlab_token", prefix: "glpat-", body: Body::UrlSafe, min: 20, max: 40 },
    TokenRule { id: "slack_token", prefix: "xoxb-", body: Body::UrlSafe, min: 20, max: 200 },
    TokenRule { id: "slack_user_token", prefix: "xoxp-", body: Body::UrlSafe, min: 20, max: 200 },
    TokenRule { id: "google_api_key", prefix: "AIza", body: Body::UrlSafe, min: 35, max: 35 },
    TokenRule { id: "stripe_live_key", prefix: "sk_live_", body: Body::Alnum, min: 20, max: 120 },
    TokenRule { id: "openai_api_key", prefix: "sk-proj-", body: Body::UrlSafe, min: 40, max: 200 },
    TokenRule { id: "anthropic_api_key", prefix: "sk-ant-", body: Body::UrlSafe, min: 40, max: 200 },
    TokenRule { id: "elitea_token", prefix: "elnat_", body: Body::UrlSafe, min: 20, max: 200 },
];

/// Connection-string schemes whose URL user-info is a database or broker
/// password.
const CONNECTION_SCHEMES: &[&str] = &[
    "postgres://", "postgresql://", "mysql://", "mariadb://", "mongodb://", "mongodb+srv://",
    "redis://", "rediss://", "amqp://", "amqps://", "mssql://", "sqlserver://",
];

/// Placeholder passwords a template or a test fixture uses.
const PLACEHOLDER_PASSWORDS: &[&str] = &[
    "password", "pass", "secret", "changeme", "example", "xxx", "xxxx", "****", "${password}",
    "$password", "<password>", "{password}", "pw", "test",
];

/// One finding: the rule and the line. The path stays in memory for the
/// turn's failure only; it is not logged.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct Finding {
    pub(crate) path: PathBuf,
    pub(crate) line: usize,
    pub(crate) rule: &'static str,
}

/// Scan bounds: files larger than `max_file_bytes` are not read (the caps
/// already refused them at send), nor more than `max_files` files.
#[derive(Clone, Copy, Debug)]
pub(crate) struct ScanLimits {
    pub(crate) max_file_bytes: u64,
    pub(crate) max_files: u64,
}

/// Why the scan could not complete. A scan that could not complete fails the
/// turn like a finding does: an unscanned tree is never handed to a sandbox.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ScanError {
    Io,
    TooManyFiles,
}

/// Scan every regular file under `root` (symlinks are not followed, `.git`
/// is skipped, and so is every path in `skip`: the explicit includes of
/// contract 6.2, which the person confirmed at send). Returns every
/// finding, in path order.
///
/// # Errors
///
/// The tree cannot be read, or holds more than `max_files` files.
pub(crate) fn scan_tree(
    root: &Path,
    limits: ScanLimits,
    skip: &std::collections::BTreeSet<PathBuf>,
) -> Result<Vec<Finding>, ScanError> {
    let mut findings = Vec::new();
    let mut files: u64 = 0;
    let mut stack = vec![root.to_path_buf()];
    while let Some(directory) = stack.pop() {
        let mut entries: Vec<_> = std::fs::read_dir(&directory)
            .map_err(|_| ScanError::Io)?
            .collect::<Result<_, _>>()
            .map_err(|_| ScanError::Io)?;
        entries.sort_by_key(std::fs::DirEntry::file_name);
        for entry in entries.into_iter().rev() {
            let kind = entry.file_type().map_err(|_| ScanError::Io)?;
            let path = entry.path();
            if kind.is_dir() {
                if entry.file_name() != ".git" {
                    stack.push(path);
                }
                continue;
            }
            if !kind.is_file() {
                continue;
            }
            files += 1;
            if files > limits.max_files {
                return Err(ScanError::TooManyFiles);
            }
            let relative = path.strip_prefix(root).unwrap_or(&path).to_path_buf();
            if skip.contains(&relative) {
                continue;
            }
            let size = entry.metadata().map_err(|_| ScanError::Io)?.len();
            if size > limits.max_file_bytes {
                continue;
            }
            let content = std::fs::read(&path).map_err(|_| ScanError::Io)?;
            for (line, rule) in scan_bytes(&content) {
                findings.push(Finding {
                    path: path.strip_prefix(root).unwrap_or(&path).to_path_buf(),
                    line,
                    rule,
                });
            }
        }
    }
    findings.sort_by(|left, right| (&left.path, left.line).cmp(&(&right.path, right.line)));
    Ok(findings)
}

/// The findings in one file's bytes, as (1-based line, rule). Binary content
/// (a NUL in the first 8000 bytes, as git decides) is not scanned.
#[must_use]
pub(crate) fn scan_bytes(content: &[u8]) -> Vec<(usize, &'static str)> {
    if content[..content.len().min(8000)].contains(&0) {
        return Vec::new();
    }
    let mut findings = Vec::new();
    for (index, line) in content.split(|byte| *byte == b'\n').enumerate() {
        if let Some(rule) = scan_line(line) {
            findings.push((index + 1, rule));
        }
    }
    findings
}

fn scan_line(line: &[u8]) -> Option<&'static str> {
    if private_key_armour(line) {
        return Some("private_key");
    }
    for rule in RULES {
        if token_match(line, rule) {
            return Some(rule.id);
        }
    }
    if connection_string_with_password(line) {
        return Some("connection_string_password");
    }
    None
}

fn private_key_armour(line: &[u8]) -> bool {
    let Some(start) = find(line, b"-----BEGIN ") else {
        return false;
    };
    let rest = &line[start + 11..];
    let Some(end) = find(rest, b"-----") else {
        return false;
    };
    let label = &rest[..end];
    label.ends_with(b"PRIVATE KEY") || label == b"PGP PRIVATE KEY BLOCK"
}

fn token_match(line: &[u8], rule: &TokenRule) -> bool {
    let prefix = rule.prefix.as_bytes();
    let mut from = 0;
    while let Some(offset) = find(&line[from..], prefix) {
        let start = from + offset;
        let before_ok = start == 0 || !rule.body.admits(line[start - 1]);
        let body_start = start + prefix.len();
        let length = line[body_start..]
            .iter()
            .take_while(|byte| rule.body.admits(**byte))
            .count();
        if before_ok && (rule.min..=rule.max).contains(&length) {
            return true;
        }
        from = start + 1;
    }
    false
}

fn connection_string_with_password(line: &[u8]) -> bool {
    let lower = line.to_ascii_lowercase();
    for scheme in CONNECTION_SCHEMES {
        let mut from = 0;
        while let Some(offset) = find(&lower[from..], scheme.as_bytes()) {
            let start = from + offset + scheme.len();
            let authority_end = lower[start..]
                .iter()
                .position(|byte| matches!(byte, b'/' | b' ' | b'"' | b'\'' | b'\t' | b'`'))
                .map_or(lower.len(), |end| start + end);
            let authority = &line[start..authority_end];
            if let Some(at) = authority.iter().rposition(|byte| *byte == b'@')
                && let Some(colon) = authority[..at].iter().position(|byte| *byte == b':')
            {
                let password = &authority[colon + 1..at];
                let placeholder = PLACEHOLDER_PASSWORDS
                    .iter()
                    .any(|candidate| password.eq_ignore_ascii_case(candidate.as_bytes()));
                let templated = password.starts_with(b"${") || password.starts_with(b"{{");
                if !password.is_empty() && !placeholder && !templated {
                    return true;
                }
            }
            from = start;
        }
    }
    false
}

fn find(haystack: &[u8], needle: &[u8]) -> Option<usize> {
    if needle.is_empty() || haystack.len() < needle.len() {
        return None;
    }
    haystack
        .windows(needle.len())
        .position(|window| window == needle)
}

#[cfg(test)]
mod tests {
    use super::{ScanLimits, scan_bytes, scan_tree};

    #[test]
    fn credential_shapes_are_found_with_their_line() {
        let text = "x = 1\naws_key = \"AKIAABCDEFGHIJKLMNOP\"\n";
        assert_eq!(scan_bytes(text.as_bytes()), [(2, "aws_access_key_id")]);
        let key = "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n";
        assert_eq!(scan_bytes(key.as_bytes()), [(1, "private_key")]);
        let github = format!("token: ghp_{}", "a".repeat(36));
        assert_eq!(scan_bytes(github.as_bytes()), [(1, "github_token")]);
        let url = "DATABASE_URL=postgres://app:s3cr3t-Value@db.internal:5432/app";
        assert_eq!(scan_bytes(url.as_bytes()), [(1, "connection_string_password")]);
    }

    #[test]
    fn placeholders_and_near_misses_are_not_findings() {
        for text in [
            "DATABASE_URL=postgres://user:password@localhost/db",
            "url = \"postgres://${DB_USER}:${DB_PASSWORD}@db/app\"",
            "postgres://localhost/db",
            "AKIA_SHORT",
            "-----BEGIN PUBLIC KEY-----",
            "ghp_tooShort",
        ] {
            assert!(scan_bytes(text.as_bytes()).is_empty(), "{text}");
        }
        // Binary content is not scanned.
        let mut binary = b"AKIAABCDEFGHIJKLMNOP".to_vec();
        binary.insert(0, 0);
        assert!(scan_bytes(&binary).is_empty());
    }

    #[test]
    fn a_tree_scan_skips_git_and_reports_relative_paths() {
        let root = tempfile::tempdir().expect("dir");
        std::fs::create_dir_all(root.path().join(".git")).expect("git");
        std::fs::write(root.path().join(".git/config"), "AKIAABCDEFGHIJKLMNOP").expect("w");
        std::fs::create_dir_all(root.path().join("src")).expect("src");
        std::fs::write(root.path().join("src/ok.rs"), "fn main() {}\n").expect("w");
        std::fs::write(
            root.path().join("src/leak.env"),
            "\n\nKEY=AKIAABCDEFGHIJKLMNOP\n",
        )
        .expect("w");
        let findings = scan_tree(
            root.path(),
            ScanLimits {
                max_file_bytes: 1 << 20,
                max_files: 10,
            },
            &std::collections::BTreeSet::new(),
        )
        .expect("scan");
        assert_eq!(findings.len(), 1);
        // An explicit include of the send is not re-scanned.
        let skipped = scan_tree(
            root.path(),
            ScanLimits {
                max_file_bytes: 1 << 20,
                max_files: 10,
            },
            &std::collections::BTreeSet::from([std::path::PathBuf::from("src/leak.env")]),
        )
        .expect("scan");
        assert!(skipped.is_empty());
        assert_eq!(findings[0].path, std::path::Path::new("src/leak.env"));
        assert_eq!(findings[0].line, 3);
        assert!(
            scan_tree(
                root.path(),
                ScanLimits {
                    max_file_bytes: 1 << 20,
                    max_files: 1,
                },
                &std::collections::BTreeSet::new(),
            )
            .is_err()
        );
    }
}
