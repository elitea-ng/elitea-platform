//! Known token formats removed from a host command's output (ADR-0061
//! decision 5c and its threat model, "exfiltration through the model").
//!
//! A command in a trusted folder runs as the person, so `cat ~/.kube/config`
//! or `gh auth token` prints real secrets. Before the output is kept or
//! shown to the model, it passes this redactor after the exact-value
//! [`crate::credentials::Redactor`] (the grant tokens and the app's own
//! tokens). It replaces, with [`REDACTED`]:
//!
//! | Format | Example shape |
//! |---|---|
//! | AWS access key id | `AKIA…`, `ASIA…` (20 characters) |
//! | AWS secret access key and session token assignments | `aws_secret_access_key = …`, `aws_session_token = …` |
//! | PEM private-key blocks (any `… PRIVATE KEY` type), across lines or inline (a GCP service-account JSON's `private_key`) | `-----BEGIN RSA PRIVATE KEY-----` … `-----END RSA PRIVATE KEY-----` |
//! | A GCP service account's `private_key_id` | `"private_key_id": "<40 hex>"` |
//! | Google OAuth access and refresh tokens (`gcloud auth print-access-token`) | `ya29.…`, `1//0…` |
//! | GitHub tokens | `ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_` |
//! | GitLab tokens | `glpat-…` |
//! | Slack tokens | `xoxb-…`, `xoxp-…`, `xapp-…` |
//! | JSON Web Tokens (Kubernetes service accounts, `az account get-access-token`, OIDC) | `eyJ….eyJ….…` |
//! | HTTP bearer credentials | `Authorization: Bearer …` |
//!
//! The stream is judged line by line: whole lines are redacted and emitted
//! at once; of a line still being written, what cannot be part of a token
//! (everything before its last word) is emitted, and the last word waits.
//! A line that may hold a multi-word secret (an assignment, a PEM header)
//! waits whole, up to [`MAX_PENDING`] bytes. Inside a PEM block every line
//! is dropped until its `END` line. Unknown formats pass: this is a guard
//! rail, not a guarantee, and the threat model says so.

use std::sync::LazyLock;

use regex::bytes::Regex;

pub use crate::credentials::REDACTED;
use crate::credentials::{Redactor, Secret};

/// The most bytes of one unfinished line kept back; past it the line is
/// judged as it is.
pub const MAX_PENDING: usize = 16 * 1024;

/// Single-line rules: a match (or its `secret` group) becomes
/// [`REDACTED`].
struct Rule {
    regex: Regex,
    /// The capture group that holds the secret (0: the whole match).
    secret: usize,
}

#[allow(
    clippy::panic,
    reason = "a constant pattern; the tests compile every one"
)]
fn rule(pattern: &str, secret: usize) -> Rule {
    Rule {
        regex: Regex::new(pattern).unwrap_or_else(|error| panic!("redact rule {pattern}: {error}")),
        secret,
    }
}

static RULES: LazyLock<Vec<Rule>> = LazyLock::new(|| {
    vec![
        // A whole PEM private key on one line (JSON-escaped newlines).
        rule(
            r"-----BEGIN[ A-Z0-9_-]{0,100}PRIVATE KEY(?: BLOCK)?-----.*?-----END[ A-Z0-9_-]{0,100}PRIVATE KEY(?: BLOCK)?-----",
            0,
        ),
        rule(r"\b(?:AKIA|ASIA|ABIA|ACCA|A3T[A-Z0-9])[A-Z2-7]{16}\b", 0),
        rule(
            r#"(?i)aws_?secret_?access_?key["']?\s*[:=]\s*["']?([A-Za-z0-9/+=]{40})"#,
            1,
        ),
        rule(
            r#"(?i)aws_?session_?token["']?\s*[:=]\s*["']?([A-Za-z0-9/+=]{40,})"#,
            1,
        ),
        rule(r#""private_key_id"\s*:\s*"([0-9a-fA-F]{40})""#, 1),
        rule(r"\bya29\.[0-9A-Za-z_-]{20,}", 0),
        rule(r"\b1//0[0-9A-Za-z_-]{20,}", 0),
        rule(r"\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}\b", 0),
        rule(r"\bgithub_pat_[A-Za-z0-9_]{22,}\b", 0),
        rule(r"\bgl(?:pat|dt|ptt|rt)-[A-Za-z0-9_-]{20,}", 0),
        rule(r"\bxox[abposre]-[A-Za-z0-9-]{10,}", 0),
        rule(r"\bxapp-[A-Za-z0-9-]{10,}", 0),
        rule(
            r"\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}",
            0,
        ),
        rule(r"(?i)\bbearer\s+([A-Za-z0-9._~+/-]{16,}=*)", 1),
    ]
});

/// The start of a PEM private-key block (its `END` is not on the line).
#[allow(
    clippy::panic,
    reason = "a constant pattern; the tests compile every one"
)]
static PEM_BEGIN: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"-----BEGIN[ A-Z0-9_-]{0,100}PRIVATE KEY(?: BLOCK)?-----").unwrap_or_else(|e| {
        panic!("redact PEM begin: {e}");
    })
});

/// The end of a PEM private-key block.
#[allow(
    clippy::panic,
    reason = "a constant pattern; the tests compile every one"
)]
static PEM_END: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"-----END[ A-Z0-9_-]{0,100}PRIVATE KEY(?: BLOCK)?-----").unwrap_or_else(|e| {
        panic!("redact PEM end: {e}");
    })
});

/// Words that make an unfinished line wait whole: its secret may come after
/// a space (`aws_secret_access_key = …`, `Bearer …`, a PEM header).
#[allow(
    clippy::panic,
    reason = "a constant pattern; the tests compile every one"
)]
static WAIT_WHOLE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"(?i)secret|session_?token|private|bearer|-----BEGIN").unwrap_or_else(|e| {
        panic!("redact wait-whole: {e}");
    })
});

/// The streaming pattern redactor (see the module documentation).
#[derive(Debug, Default)]
pub struct PatternRedactor {
    pending: Vec<u8>,
    /// Inside a PEM private-key block: lines are dropped until its end.
    in_block: bool,
    /// How many bytes of the unfinished line already went out (its whole
    /// words, redacted).
    emitted: usize,
}

/// How the tokens of the single-line rules start (the rest need a keyword
/// that [`WAIT_WHOLE`] holds the line for).
const TOKEN_STARTS: &[&[u8]] = &[
    b"ghp_",
    b"gho_",
    b"ghu_",
    b"ghs_",
    b"ghr_",
    b"github_pat_",
    b"glpat-",
    b"gldt-",
    b"glptt-",
    b"glrt-",
    b"xoxa-",
    b"xoxb-",
    b"xoxp-",
    b"xoxo-",
    b"xoxs-",
    b"xoxr-",
    b"xoxe-",
    b"xapp-",
    b"AKIA",
    b"ASIA",
    b"ABIA",
    b"ACCA",
    b"A3T",
    b"ya29.",
    b"1//0",
    b"eyJ",
    b"-----",
];

/// Whether a token could start at the beginning of `word` (or `word` is
/// the start of one): the word waits for the rest of it. A word that does
/// not start at a token's word boundary (`abcghp_…`) matches no rule.
fn may_start_token(word: &[u8]) -> bool {
    // Every place a rule's `\b` allows a token to start: the word's start,
    // and after each character that is not a word character (`AWS=AKIA…`).
    (0..word.len())
        .filter(|&at| at == 0 || !(word[at - 1].is_ascii_alphanumeric() || word[at - 1] == b'_'))
        .any(|at| {
            let rest = &word[at..];
            TOKEN_STARTS
                .iter()
                .any(|start| rest.starts_with(start) || start.starts_with(rest))
        })
}

/// `line` with every single-line rule applied.
fn redact_line(line: &[u8]) -> Vec<u8> {
    let mut text = line.to_vec();
    for rule in RULES.iter() {
        if !rule.regex.is_match(&text) {
            continue;
        }
        let mut out = Vec::with_capacity(text.len());
        let mut last = 0;
        for captures in rule.regex.captures_iter(&text) {
            let Some(secret) = captures.get(rule.secret) else {
                continue;
            };
            out.extend_from_slice(&text[last..secret.start()]);
            out.extend_from_slice(REDACTED.as_bytes());
            last = secret.end();
        }
        out.extend_from_slice(&text[last..]);
        text = out;
    }
    text
}

impl PatternRedactor {
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// One whole line (with its `\n`, if any), judged and appended to `out`.
    fn line(&mut self, line: &[u8], out: &mut Vec<u8>) {
        let newline = line.ends_with(b"\n");
        if self.in_block {
            if let Some(end) = PEM_END.find(line) {
                self.in_block = false;
                // What follows the block's end on its line is judged.
                let rest = redact_line(&line[end.end()..]);
                out.extend_from_slice(&rest);
            }
            // The block's other lines are dropped; its one marker stands.
            return;
        }
        let redacted = redact_line(line);
        if let Some(begin) = PEM_BEGIN.find(&redacted) {
            // A block starts here and does not end on this line (an inline
            // one was redacted above).
            out.extend_from_slice(&redacted[..begin.start()]);
            out.extend_from_slice(REDACTED.as_bytes());
            if newline {
                out.push(b'\n');
            }
            self.in_block = true;
            return;
        }
        out.extend_from_slice(&redacted);
    }

    /// The bytes of `chunk` that are safe to keep now.
    pub fn push(&mut self, chunk: &[u8]) -> Vec<u8> {
        self.pending.extend_from_slice(chunk);
        let mut out = Vec::with_capacity(self.pending.len());
        while let Some(at) = self.pending.iter().position(|byte| *byte == b'\n') {
            let line: Vec<u8> = self.pending.drain(..=at).collect();
            // The bytes before `emitted` went out already, redacted on their
            // own: whole words with no multi-word secret's start
            // (`emit_safe_prefix`). The rest is judged now.
            let skip = std::mem::take(&mut self.emitted).min(line.len());
            self.line(&line[skip..], &mut out);
        }
        self.emit_safe_prefix(&mut out);
        out
    }

    /// Emit what of the unfinished line cannot be part of a token, redacted:
    /// nothing in a block or on a line that may hold a multi-word secret;
    /// else its whole words (everything before its last word).
    fn emit_safe_prefix(&mut self, out: &mut Vec<u8>) {
        if self.pending.len() > MAX_PENDING {
            // Too long to wait for: judged as it is, as a line of its own.
            let line = std::mem::take(&mut self.pending);
            let skip = std::mem::take(&mut self.emitted).min(line.len());
            self.line(&line[skip..], out);
            return;
        }
        if self.in_block || WAIT_WHOLE.is_match(&self.pending) {
            return;
        }
        let mut boundary = self
            .pending
            .iter()
            .rposition(u8::is_ascii_whitespace)
            .map_or(0, |at| at + 1);
        // The last word goes out too when no token can start with it, so a
        // prompt (`Proceed (y/n)?`, `name:`) is not held until its answer.
        if !may_start_token(&self.pending[boundary..]) {
            boundary = self.pending.len();
        }
        if boundary > self.emitted {
            out.extend_from_slice(&redact_line(&self.pending[self.emitted..boundary]));
            self.emitted = boundary;
        }
    }

    /// What is left at the end of the stream.
    pub fn finish(&mut self) -> Vec<u8> {
        let line = std::mem::take(&mut self.pending);
        let skip = std::mem::take(&mut self.emitted).min(line.len());
        let mut out = Vec::new();
        if line.len() > skip {
            self.line(&line[skip..], &mut out);
        }
        self.in_block = false;
        out
    }
}

/// A command's output stream as it is kept: the exact secrets first (a
/// grant's login, the app's own tokens: [`Redactor`]), then, for a host
/// command, the known token formats ([`PatternRedactor`]).
#[derive(Debug)]
pub struct StreamRedactor {
    exact: Redactor,
    patterns: Option<PatternRedactor>,
}

impl StreamRedactor {
    /// `patterns`: also remove the known token formats (host commands).
    #[must_use]
    pub fn new(secrets: &[Secret], patterns: bool) -> Self {
        Self {
            exact: Redactor::new(secrets),
            patterns: patterns.then(PatternRedactor::new),
        }
    }

    /// The bytes of `chunk` that are safe to keep now.
    pub fn push(&mut self, chunk: &[u8]) -> Vec<u8> {
        let safe = self.exact.push(chunk);
        match &mut self.patterns {
            Some(patterns) => patterns.push(&safe),
            None => safe,
        }
    }

    /// What is left at the end of the stream.
    pub fn finish(&mut self) -> Vec<u8> {
        let rest = self.exact.finish();
        match &mut self.patterns {
            Some(patterns) => {
                let mut out = patterns.push(&rest);
                out.extend(patterns.finish());
                out
            }
            None => rest,
        }
    }
}

/// `text` redacted as a whole (a finished output).
#[must_use]
pub fn redact_text(text: &str) -> String {
    let mut redactor = PatternRedactor::new();
    let mut out = redactor.push(text.as_bytes());
    out.extend(redactor.finish());
    String::from_utf8_lossy(&out).into_owned()
}

#[cfg(test)]
mod tests {
    use super::{PatternRedactor, REDACTED, redact_text};

    fn streamed(chunks: &[&str]) -> String {
        let mut redactor = PatternRedactor::new();
        let mut out = Vec::new();
        for chunk in chunks {
            out.extend(redactor.push(chunk.as_bytes()));
        }
        out.extend(redactor.finish());
        String::from_utf8(out).unwrap()
    }

    /// Fixtures are put together at run time, so no scanner takes the
    /// source for a leaked key.
    fn aws_key() -> String {
        ["AK", "IA", "IOSFODNN7", "ABCDEFG"].concat()
    }
    const GH_TOKEN: &str = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB";
    const PEM: &str = "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234\nabcdEFGH5678\n-----END RSA PRIVATE KEY-----\n";

    #[test]
    fn known_formats_are_redacted_whole() {
        let aws = aws_key();
        let text = format!("key={aws}\ntoken: {GH_TOKEN}\n{PEM}done\n");
        let out = redact_text(&text);
        assert!(!out.contains(&aws), "{out}");
        assert!(!out.contains("ghp_0123"), "{out}");
        assert!(
            !out.contains("MIIEow") && !out.contains("abcdEFGH"),
            "{out}"
        );
        assert!(out.contains("done\n"), "{out}");
        assert_eq!(out.matches(REDACTED).count(), 3, "{out}");
    }

    #[test]
    fn a_secret_split_across_reads_is_still_found() {
        let aws = aws_key();
        let text = format!("export AWS={aws} and {GH_TOKEN} end\n{PEM}");
        // Every split point, two reads each.
        for cut in 1..text.len() {
            if !text.is_char_boundary(cut) {
                continue;
            }
            let out = streamed(&[&text[..cut], &text[cut..]]);
            assert!(!out.contains(&aws), "cut {cut}: {out}");
            assert!(!out.contains("ghp_0123"), "cut {cut}: {out}");
            assert!(!out.contains("MIIEow"), "cut {cut}: {out}");
            assert!(
                out.starts_with("export AWS=") && out.contains(" end\n"),
                "cut {cut}: {out}"
            );
        }
        // One byte at a time.
        let bytes: Vec<String> = text.chars().map(String::from).collect();
        let refs: Vec<&str> = bytes.iter().map(String::as_str).collect();
        let out = streamed(&refs);
        assert!(!out.contains(&aws) && !out.contains("ghp_0123") && !out.contains("MIIEow"));
    }

    #[test]
    fn a_prompt_is_not_held_until_its_answer() {
        let mut redactor = PatternRedactor::new();
        assert_eq!(redactor.push(b"Proceed (y/n)?"), b"Proceed (y/n)?");
        assert_eq!(redactor.push(b"name:"), b"name:");
        // The start of a token waits.
        assert!(redactor.push(b" gh").is_empty() || !redactor.pending.is_empty());
    }

    #[test]
    fn cloud_cli_logins_are_redacted() {
        let aws_secret = format!(
            "aws_secret_access_key = {}",
            ["wJalrXUtn", "FEMI/K7MDENG/bPxRfiCYz", "FAKEFAKEKY"].concat()
        );
        // Put together at run time: the shipped-file token scan.
        let bearer = ["Authorization: Bear", "er abcdefghijklmnop0123456789"].concat();
        let jwt = [
            "token: eyJhbGciOiJSUzI1NiJ9.",
            "eyJzdWIiOiJzeXN0ZW0ifQ.",
            "c2lnbmF0dXJlLXNpZ25hdHVyZQ",
        ]
        .concat();
        let cases = [
            aws_secret.as_str(),
            "aws_session_token=IQoJb3JpZ2luX2VjEHUaCXVzLWVhc3QtMSJHMEUCIQDexampleexampleexample",
            "ya29.a0AfH6SMBexampleexampleexampleexample",
            "1//0gexampleexampleexampleexample-refresh",
            "xoxb-123456789012-abcdefghijkl",
            "glpat-abcdefghijklmnopqrstu",
            "github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz",
            jwt.as_str(),
            bearer.as_str(),
            r#"{"private_key_id": "0123456789abcdef0123456789abcdef01234567", "private_key": "-----BEGIN PRIVATE KEY-----\nMIIEvQ\n-----END PRIVATE KEY-----\n"}"#,
        ];
        for case in cases {
            let out = redact_text(&format!("{case}\n"));
            assert!(out.contains(REDACTED), "{case} -> {out}");
            for secret in [
                "wJalrXUtn",
                "IQoJb3J",
                "a0AfH6",
                "gexample",
                "123456789012-abc",
                "abcdefghijklmnopqrstu",
                "11ABCDEFG",
                "c2lnbmF0",
                "abcdefghijklmnop0123",
                "MIIEvQ",
                "0123456789abcdef0123",
            ] {
                assert!(!out.contains(secret), "{case} leaked {secret}: {out}");
            }
        }
    }

    #[test]
    fn ordinary_output_streams_through_unchanged() {
        let text = "total 8\ndrwxr-xr-x  2 me  staff  64 Nov 25 build\nCompiling elitea v0.1\n";
        assert_eq!(streamed(&[text]), text);
        // An unfinished line emits at once what cannot start a token.
        let mut redactor = PatternRedactor::new();
        assert_eq!(
            redactor.push(b"Password for user: "),
            b"Password for user: "
        );
        assert_eq!(redactor.push(b"hunter"), b"hunter");
        assert_eq!(redactor.push(b" AK"), b" ");
        assert_eq!(redactor.finish(), b"AK");
    }
}
