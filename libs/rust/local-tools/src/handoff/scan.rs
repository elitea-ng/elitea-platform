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
/// A file with a NUL in its first bytes is binary and not scanned.
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

/// The findings in one file's contents.
///
/// # Errors
///
/// The shipped rules do not compile.
pub fn scan(path: &str, bytes: &[u8]) -> ToolResult<Vec<Finding>> {
    if is_binary(bytes) {
        return Ok(Vec::new());
    }
    let text = String::from_utf8_lossy(&bytes[..bytes.len().min(MAX_SCANNED)]);
    let mut out: Vec<Finding> = Vec::new();
    for (index, line) in text.lines().enumerate() {
        for rule in rules()? {
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
    fn findings_name_the_line_and_never_the_secret_and_binaries_are_skipped() {
        let text = join(&["line one\nline two\nkey=AKIA", "Z7QWERTYUIOPASDF\n"]);
        let findings = scan("cfg/app.ini", text.as_bytes()).expect("scan");
        assert_eq!(findings.len(), 1);
        assert_eq!(findings[0].path, "cfg/app.ini");
        assert_eq!(findings[0].line, 3);
        assert!(!format!("{findings:?}").contains("QWERTY"));
        let mut binary = text.into_bytes();
        binary.insert(0, 0);
        assert!(scan("b.bin", &binary).expect("scan").is_empty());
    }
}
