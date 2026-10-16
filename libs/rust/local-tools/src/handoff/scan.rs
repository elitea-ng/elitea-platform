//! The secret scan of a handoff (ADR-0034 decision 5): every file the send
//! would transfer is scanned for credential-shaped content before upload.
//! The rules ship as data (`secret_rules.json`, compiled in), with no
//! network service. A finding blocks the send and names the path, the line
//! and the rule; it is never overridden. The person resolves it by leaving
//! the file on the laptop (it joins the exclusions) or by editing it.
//! Findings never carry the matched text.

use std::sync::OnceLock;

use regex::Regex;
use serde::{Deserialize, Serialize};

use crate::error::{ErrorCode, ToolError, ToolResult};

const RULES_JSON: &str = include_str!("secret_rules.json");

/// Bytes of a file that are scanned (the per-file cap is lower).
const MAX_SCANNED: usize = 32 * 1024 * 1024;
/// A file with a NUL in its first bytes is binary (scanned by the
/// high-confidence rules only, unless a UTF-16 BOM marks it as text).
const BINARY_SNIFF: usize = 8 * 1024;
/// Findings reported per file.
const MAX_FINDINGS_PER_FILE: usize = 20;

#[derive(Deserialize)]
struct RuleFile {
    rules: Vec<RuleData>,
}

#[derive(Deserialize)]
struct RuleData {
    id: String,
    description: String,
    regex: String,
    secret_group: usize,
    allow: Vec<String>,
}

struct Rule {
    id: String,
    description: String,
    regex: Regex,
    group: usize,
    allow: Vec<Regex>,
}

/// One finding: where, and which rule. Never the secret.
#[derive(Clone, Debug, Deserialize, Eq, Ord, PartialEq, PartialOrd, Serialize)]
pub struct Finding {
    pub path: String,
    /// 1-based.
    pub line: usize,
    pub rule: String,
}

fn compile() -> ToolResult<Vec<Rule>> {
    let file: RuleFile = serde_json::from_str(RULES_JSON)
        .map_err(|_| ToolError::new(ErrorCode::Io, "the secret rules cannot be read"))?;
    file.rules
        .into_iter()
        .map(|rule| {
            let bad = |what: &str| {
                ToolError::new(
                    ErrorCode::Io,
                    format!("the secret rule {} has a bad {what}", rule.id),
                )
            };
            Ok(Rule {
                regex: Regex::new(&rule.regex).map_err(|_| bad("pattern"))?,
                allow: rule
                    .allow
                    .iter()
                    .map(|allow| Regex::new(allow).map_err(|_| bad("allow pattern")))
                    .collect::<ToolResult<_>>()?,
                id: rule.id,
                description: rule.description,
                group: rule.secret_group,
            })
        })
        .collect()
}

fn rules() -> ToolResult<&'static [Rule]> {
    static RULES: OnceLock<Result<Vec<Rule>, ToolError>> = OnceLock::new();
    match RULES.get_or_init(compile) {
        Ok(rules) => Ok(rules),
        Err(error) => Err(error.clone()),
    }
}

/// The rule ids and descriptions (for the send dialog).
///
/// # Errors
///
/// The shipped rules do not compile (a build defect; tests catch it).
pub fn rule_list() -> ToolResult<Vec<(String, String)>> {
    Ok(rules()?
        .iter()
        .map(|rule| (rule.id.clone(), rule.description.clone()))
        .collect())
}

/// Whether `bytes` look binary.
#[must_use]
pub fn is_binary(bytes: &[u8]) -> bool {
    bytes[..bytes.len().min(BINARY_SNIFF)].contains(&0)
}

/// The rules that also run over the raw bytes of a binary file: the
/// high-confidence, fixed-shape credentials.
const BINARY_RULES: &[&str] = &[
    "private-key",
    "aws-access-key-id",
    "github-token",
    "github-fine-grained-token",
    "slack-token",
    "gitlab-token",
];

/// A UTF-16 text (a byte-order mark first), decoded; `None` for anything
/// else. Windows tools write configs this way, and their NULs would
/// otherwise mark them binary.
fn utf16_text(bytes: &[u8]) -> Option<String> {
    let (little, body) = match bytes {
        [0xFF, 0xFE, rest @ ..] => (true, rest),
        [0xFE, 0xFF, rest @ ..] => (false, rest),
        _ => return None,
    };
    let units = body.chunks_exact(2).map(|pair| {
        if little {
            u16::from_le_bytes([pair[0], pair[1]])
        } else {
            u16::from_be_bytes([pair[0], pair[1]])
        }
    });
    Some(
        char::decode_utf16(units)
            .map(|c| c.unwrap_or('\u{FFFD}'))
            .collect(),
    )
}

/// The findings in one file's contents. A UTF-16 text (a BOM) is decoded
/// and scanned as text. Another binary file is scanned over its raw bytes
/// by the high-confidence rules only (private-key headers, AWS, GitHub,
/// Slack and GitLab tokens). At most the first 32 MiB are read.
///
/// # Errors
///
/// The shipped rules do not compile.
pub fn scan(path: &str, bytes: &[u8]) -> ToolResult<Vec<Finding>> {
    let bytes = &bytes[..bytes.len().min(MAX_SCANNED)];
    if let Some(text) = utf16_text(bytes) {
        return scan_text(path, &text, None);
    }
    if is_binary(bytes) {
        // One char per byte, so every ASCII-shaped secret in the blob matches.
        let text: String = bytes.iter().map(|&byte| char::from(byte)).collect();
        return scan_text(path, &text, Some(BINARY_RULES));
    }
    scan_text(path, &String::from_utf8_lossy(bytes), None)
}

fn scan_text(path: &str, text: &str, only: Option<&[&str]>) -> ToolResult<Vec<Finding>> {
    let mut out: Vec<Finding> = Vec::new();
    for (index, line) in text.lines().enumerate() {
        for rule in rules()? {
            if only.is_some_and(|ids| !ids.contains(&rule.id.as_str())) {
                continue;
            }
            let hit = rule.regex.captures_iter(line).any(|captures| {
                captures.get(rule.group).is_some_and(|secret| {
                    !rule
                        .allow
                        .iter()
                        .any(|allow| allow.is_match(secret.as_str()))
                })
            });
            if hit {
                out.push(Finding {
                    path: path.to_owned(),
                    line: index + 1,
                    rule: rule.id.clone(),
                });
                if out.len() >= MAX_FINDINGS_PER_FILE {
                    return Ok(out);
                }
            }
        }
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::{rule_list, scan};

    fn rules_hit(text: &str) -> Vec<String> {
        scan("f", text.as_bytes())
            .expect("scan")
            .into_iter()
            .map(|finding| finding.rule)
            .collect()
    }

    // Test fixtures are assembled at run time, so this file itself holds
    // no credential-shaped literal.
    fn join(parts: &[&str]) -> String {
        parts.concat()
    }

    #[test]
    fn the_shipped_rules_compile() {
        assert!(rule_list().expect("rules").len() >= 10);
    }

    #[test]
    fn true_positives() {
        let cases = [
            (
                join(&["key = AKIA", "Z7QWERTYUIOPASDF"]),
                "aws-access-key-id",
            ),
            (
                join(&[
                    "aws_secret_access_key = ",
                    "wJalrXUtnFEMI/K7MDENG/bPxRfiCYzzzzzzzzzz",
                ]),
                "aws-secret-access-key",
            ),
            (
                join(&["-----BEGIN RSA ", "PRIVATE KEY-----"]),
                "private-key",
            ),
            (
                join(&["-----BEGIN OPENSSH ", "PRIVATE KEY-----"]),
                "private-key",
            ),
            (join(&["-----BEGIN ", "PRIVATE KEY-----"]), "private-key"),
            (
                join(&["token: ghp_", "abcdefghijklmnopqrstuvwxyz0123456789"]),
                "github-token",
            ),
            (
                join(&["github_pat_", &"A1b2".repeat(20), "zz"]),
                "github-fine-grained-token",
            ),
            (join(&["GL=glpat-", "abcdefghij0123456789"]), "gitlab-token"),
            (
                join(&["SLACK=xoxb-", "1234567890-abcdefghij"]),
                "slack-token",
            ),
            (
                join(&[
                    "https://hooks.slack.com/services/",
                    "T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX",
                ]),
                "slack-webhook",
            ),
            (
                join(&[
                    "DATABASE_URL=postgres://app:",
                    "s3cr3t-pw@db.internal:5432/app",
                ]),
                "connection-string-password",
            ),
            (
                join(&["Server=db;User Id=sa;Password=", "Hunter2Hunter2;"]),
                "password-assignment",
            ),
            (
                join(&["password: \"", "correct-horse-battery\""]),
                "password-assignment",
            ),
            (
                join(&[
                    "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9",
                    ".eyJzdWIiOiIxMjM0NTY3ODkwIn0",
                    ".dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
                ]),
                "jwt",
            ),
        ];
        for (text, rule) in &cases {
            let hit = rules_hit(text);
            assert!(
                hit.iter().any(|seen| seen == rule),
                "{rule} not found in {text:?}: {hit:?}"
            );
        }
    }

    #[test]
    fn false_positives_are_not_findings() {
        let cases = [
            join(&["AKIAIOSFODNN7", "EXAMPLE"]),
            "aws_secret_access_key = os.environ[\"KEY\"]".to_owned(),
            "-----BEGIN PUBLIC KEY-----".to_owned(),
            "-----BEGIN CERTIFICATE-----".to_owned(),
            "ghp_short".to_owned(),
            "postgres://user:${DB_PASSWORD}@db:5432/app".to_owned(),
            "https://user:<password>@example.com".to_owned(),
            "https://github.com/org/repo.git".to_owned(),
            "password = os.environ['PW']".to_owned(),
            "password=${PASSWORD}".to_owned(),
            "password: changeme".to_owned(),
            "let password_hash = hash(password);".to_owned(),
            "if password == expected {".to_owned(),
            "fn check_password(input: &str) -> bool".to_owned(),
            "eyJhbGciOi is a prefix only".to_owned(),
            "xoxb-short".to_owned(),
        ];
        for text in &cases {
            assert!(
                rules_hit(text).is_empty(),
                "false positive in {text:?}: {:?}",
                rules_hit(text)
            );
        }
    }

    #[test]
    fn findings_name_the_line_and_never_the_secret() {
        let text = join(&["line one\nline two\nkey=AKIA", "Z7QWERTYUIOPASDF\n"]);
        let findings = scan("cfg/app.ini", text.as_bytes()).expect("scan");
        assert_eq!(findings.len(), 1);
        assert_eq!(findings[0].path, "cfg/app.ini");
        assert_eq!(findings[0].line, 3);
        assert!(!format!("{findings:?}").contains("QWERTY"));
    }

    fn utf16(text: &str, little: bool) -> Vec<u8> {
        let mut out = if little {
            vec![0xFF, 0xFE]
        } else {
            vec![0xFE, 0xFF]
        };
        for unit in text.encode_utf16() {
            out.extend_from_slice(&if little {
                unit.to_le_bytes()
            } else {
                unit.to_be_bytes()
            });
        }
        out
    }

    #[test]
    fn a_utf16_config_is_scanned_as_text() {
        let text = join(&[
            "[db]\r\nServer=db;User Id=sa;Password=",
            "Hunter2Hunter2;\r\n",
        ]);
        for little in [true, false] {
            let findings = scan("app.config", &utf16(&text, little)).expect("scan");
            assert_eq!(
                findings
                    .iter()
                    .map(|f| (f.line, f.rule.as_str()))
                    .collect::<Vec<_>>(),
                vec![(2, "password-assignment")],
                "little endian: {little}"
            );
        }
    }

    #[test]
    fn a_binary_blob_is_scanned_for_high_confidence_keys_only() {
        let mut blob = vec![0_u8, 1, 2, 0, 0, 7, b'\n'];
        blob.extend_from_slice(join(&["\0 id=AKIA", "Z7QWERTYUIOPASDF \0"]).as_bytes());
        let findings = scan("lib/blob.dat", &blob).expect("scan");
        assert_eq!(findings.len(), 1);
        assert_eq!(findings[0].rule, "aws-access-key-id");
        assert_eq!(findings[0].path, "lib/blob.dat");
        // A rule outside the high-confidence set stays quiet in a binary.
        let mut weak = vec![0_u8, 0];
        weak.extend_from_slice(join(&["\0Password=", "Hunter2Hunter2;"]).as_bytes());
        assert!(scan("w.bin", &weak).expect("scan").is_empty());
    }

    #[test]
    fn a_plain_binary_has_no_finding() {
        let blob: Vec<u8> = (0..=255_u8).cycle().take(4096).collect();
        assert!(scan("plain.bin", &blob).expect("scan").is_empty());
        assert!(scan("p.bin", &[0, 0, 0, 0]).expect("scan").is_empty());
    }
}
