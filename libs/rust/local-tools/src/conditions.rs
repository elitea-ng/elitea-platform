//! Argument conditions on an `allow` prefix rule (#3007): what a command's
//! flags must look like, anywhere after the program, for the rule to vouch
//! for it.
//!
//! ```json
//! "conditions": {
//!   "forbid_flags": ["-f", "-F", "--field", "--raw-field", "--input"],
//!   "require_flags": ["--draft"],
//!   "flag_values": {"-X": ["GET"], "--method": ["GET"]},
//!   "graphql": "query"
//! }
//! ```
//!
//! * `forbid_flags`: none of these flags may appear.
//! * `require_flags`: each must appear as a bare flag (`--draft`, or
//!   `--draft=true`); any other attached value fails.
//! * `flag_values`: every occurrence of the flag carries one of the values
//!   (ASCII case-insensitive); absence is fine.
//! * `graphql: "query"`: the command carries at least one `query=` field
//!   (`-f`, `-F`, `--field`, `--raw-field`) and every one is a GraphQL
//!   document with no `mutation` or `subscription` operation.
//!
//! A flag is seen as `--name`, `--name=value`, `-x`, `-xVALUE`, `-x=VALUE`,
//! or inside a short-flag cluster (`-ix`). Every word is scanned on its
//! own, values included, so a word that only looks like a flag still
//! counts: the evaluator over-detects, and over-detection only makes the
//! allow not match (the call falls to the next layer, normally asked).
//! Whenever it cannot tell, the condition fails.

use std::collections::BTreeMap;

use serde::{Deserialize, Serialize};

/// The conditions of one rule. All of them must hold.
#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Conditions {
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub forbid_flags: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub require_flags: Vec<String>,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub flag_values: BTreeMap<String, Vec<String>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub graphql: Option<GraphqlOperations>,
}

/// Which GraphQL operations a `graphql` condition admits.
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum GraphqlOperations {
    /// Queries only: no `mutation`, no `subscription`.
    Query,
}

/// The fields `gh api` reads a GraphQL document from.
const QUERY_FIELD_FLAGS: &[&str] = &["-f", "-F", "--field", "--raw-field"];

impl Conditions {
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.forbid_flags.is_empty()
            && self.require_flags.is_empty()
            && self.flag_values.is_empty()
            && self.graphql.is_none()
    }

    /// Why these conditions are malformed, or `None`.
    #[must_use]
    pub fn problem(&self) -> Option<String> {
        let names = self
            .forbid_flags
            .iter()
            .chain(&self.require_flags)
            .chain(self.flag_values.keys());
        for name in names {
            if !valid_flag(name) {
                return Some(format!(
                    "`{name}` is not a flag (`-x`, one letter or digit, or `--name`)"
                ));
            }
        }
        for (flag, values) in &self.flag_values {
            if values.is_empty() || values.iter().any(String::is_empty) {
                return Some(format!("flag_values for `{flag}` needs non-empty values"));
            }
        }
        None
    }

    /// Whether the words after the program (`args`) meet every condition.
    #[must_use]
    pub fn hold(&self, args: &[String]) -> bool {
        self.forbid_flags
            .iter()
            .all(|flag| occurrences(args, flag).is_empty())
            && self.require_flags.iter().all(|flag| required(args, flag))
            && self
                .flag_values
                .iter()
                .all(|(flag, values)| values_allowed(args, flag, values))
            && self.graphql.is_none_or(|_| graphql_query_only(args))
    }
}

/// `-x` (one ASCII letter or digit) or `--name` (`[a-z0-9-]`, starting
/// with a letter or digit).
#[must_use]
pub fn valid_flag(flag: &str) -> bool {
    if let Some(long) = flag.strip_prefix("--") {
        return long
            .chars()
            .next()
            .is_some_and(|first| first.is_ascii_alphanumeric())
            && long
                .chars()
                .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-');
    }
    let mut chars = flag.chars();
    chars.next() == Some('-')
        && chars.next().is_some_and(|c| c.is_ascii_alphanumeric())
        && chars.next().is_none()
}

/// One appearance of a flag in the words.
#[derive(Clone, Debug, Eq, PartialEq)]
enum Seen {
    /// The flag alone; the next word, if any, may be its value.
    Bare { next: Option<String> },
    /// `--name=value`, `-xVALUE` or `-x=VALUE`.
    Attached(String),
    /// Inside a short-flag cluster: its value cannot be told.
    Clustered,
}

/// Every appearance of `flag` in `args`.
fn occurrences(args: &[String], flag: &str) -> Vec<Seen> {
    let mut seen = Vec::new();
    for (index, word) in args.iter().enumerate() {
        let next = || args.get(index + 1).cloned();
        if let Some(long) = flag.strip_prefix("--") {
            let Some(rest) = word.strip_prefix("--") else {
                continue;
            };
            if rest == long {
                seen.push(Seen::Bare { next: next() });
            } else if let Some(value) = rest
                .strip_prefix(long)
                .and_then(|after| after.strip_prefix('='))
            {
                seen.push(Seen::Attached(value.to_owned()));
            }
            continue;
        }
        let Some(letter) = flag.chars().nth(1) else {
            continue;
        };
        if word.starts_with("--") || !word.starts_with('-') || word.len() < 2 {
            continue;
        }
        let cluster = &word[1..];
        if cluster.starts_with(letter) {
            let value = &cluster[letter.len_utf8()..];
            if value.is_empty() {
                seen.push(Seen::Bare { next: next() });
            } else {
                seen.push(Seen::Attached(
                    value.strip_prefix('=').unwrap_or(value).to_owned(),
                ));
            }
        } else if cluster.contains(letter) {
            seen.push(Seen::Clustered);
        }
    }
    seen
}

fn required(args: &[String], flag: &str) -> bool {
    let seen = occurrences(args, flag);
    !seen.is_empty()
        && seen.iter().all(|seen| match seen {
            Seen::Bare { .. } => true,
            Seen::Attached(value) => value == "true",
            Seen::Clustered => false,
        })
}

fn values_allowed(args: &[String], flag: &str, allowed: &[String]) -> bool {
    let ok = |value: &str| allowed.iter().any(|want| want.eq_ignore_ascii_case(value));
    occurrences(args, flag).iter().all(|seen| match seen {
        Seen::Bare { next } => next.as_deref().is_some_and(ok),
        Seen::Attached(value) => ok(value),
        Seen::Clustered => false,
    })
}

/// The `query=` field values of `gh api graphql`, or `None` when one cannot
/// be read (a cluster, a field with no value, a file reference).
fn query_fields(args: &[String]) -> Option<Vec<String>> {
    let mut queries = Vec::new();
    for flag in QUERY_FIELD_FLAGS {
        for seen in occurrences(args, flag) {
            let field = match seen {
                Seen::Bare { next } => next?,
                Seen::Attached(value) => value,
                Seen::Clustered => return None,
            };
            if let Some(query) = field.strip_prefix("query=") {
                // `-F query=@file` (or `@-`) reads the text elsewhere.
                if (*flag == "-F" || *flag == "--field") && query.starts_with('@') {
                    return None;
                }
                queries.push(query.to_owned());
            }
        }
    }
    Some(queries)
}

fn graphql_query_only(args: &[String]) -> bool {
    if !occurrences(args, "--input").is_empty() {
        return false;
    }
    query_fields(args).is_some_and(|queries| {
        !queries.is_empty() && queries.iter().all(|query| document_is_query_only(query))
    })
}

/// Whether a GraphQL document has at least one operation and none that is
/// a `mutation` or a `subscription`. Comments and strings are skipped; a
/// name at brace depth 0 is an operation keyword, an operation or fragment
/// name, or a type condition, so `mutation` or `subscription` there is
/// refused wherever it stands. Unbalanced braces or an unterminated string
/// fail.
#[must_use]
pub fn document_is_query_only(document: &str) -> bool {
    let chars: Vec<char> = document.chars().collect();
    let mut depth = 0usize;
    let mut operations = 0usize;
    let mut index = 0usize;
    while index < chars.len() {
        let c = chars[index];
        match c {
            '#' => {
                while index < chars.len() && chars[index] != '\n' && chars[index] != '\r' {
                    index += 1;
                }
                continue;
            }
            '"' => {
                let Some(end) = string_end(&chars, index) else {
                    return false;
                };
                index = end;
                continue;
            }
            '{' => {
                if depth == 0 {
                    operations += 1;
                }
                depth += 1;
            }
            '}' => {
                let Some(less) = depth.checked_sub(1) else {
                    return false;
                };
                depth = less;
            }
            c if c == '_' || c.is_ascii_alphabetic() => {
                let start = index;
                while index < chars.len()
                    && (chars[index] == '_' || chars[index].is_ascii_alphanumeric())
                {
                    index += 1;
                }
                let name: String = chars[start..index].iter().collect();
                if depth == 0 && (name == "mutation" || name == "subscription") {
                    return false;
                }
                continue;
            }
            _ => {}
        }
        index += 1;
    }
    depth == 0 && operations > 0
}

/// The index just past the string starting at `start` (a `"` or `"""`),
/// or `None` when it does not end.
fn string_end(chars: &[char], start: usize) -> Option<usize> {
    let block = chars.get(start..start + 3) == Some(&['"', '"', '"']);
    let mut index = start + if block { 3 } else { 1 };
    while index < chars.len() {
        if block {
            if chars[index] == '\\' && chars.get(index + 1..index + 4) == Some(&['"', '"', '"']) {
                index += 4;
                continue;
            }
            if chars.get(index..index + 3) == Some(&['"', '"', '"']) {
                return Some(index + 3);
            }
        } else {
            match chars[index] {
                '\\' => {
                    index += 2;
                    continue;
                }
                '"' => return Some(index + 1),
                '\n' | '\r' => return None,
                _ => {}
            }
        }
        index += 1;
    }
    None
}

#[cfg(test)]
mod tests {
    use super::{Conditions, GraphqlOperations, document_is_query_only, valid_flag};

    fn args(line: &str) -> Vec<String> {
        shlex::split(line).expect("argv")
    }

    fn get_only() -> Conditions {
        Conditions {
            forbid_flags: ["-f", "-F", "--field", "--raw-field", "--input"]
                .map(str::to_owned)
                .to_vec(),
            flag_values: [
                ("-X".to_owned(), vec!["GET".to_owned()]),
                ("--method".to_owned(), vec!["GET".to_owned()]),
            ]
            .into(),
            ..Conditions::default()
        }
    }

    #[test]
    fn flag_names() {
        for good in ["-X", "-f", "-9", "--method", "--raw-field", "--x2"] {
            assert!(valid_flag(good), "{good}");
        }
        for bad in [
            "X", "-", "--", "-XY", "--Method", "--=x", "---x", "--a=b", "-é",
        ] {
            assert!(!valid_flag(bad), "{bad}");
        }
    }

    #[test]
    fn a_get_only_rule_refuses_every_spelling_of_a_write() {
        let rule = get_only();
        for read in [
            "api repos/o/r",
            "api -X GET repos/o/r",
            "api -XGET repos/o/r",
            "api -X=get repos/o/r",
            "api --method=get repos/o/r",
            "api repos/o/r --method GET --paginate",
            "api repos/o/r --jq .name",
        ] {
            assert!(rule.hold(&args(read)), "{read}");
        }
        for write in [
            "api -X POST repos/o/r/issues",
            "api -XPOST repos/o/r/issues",
            "api --method=POST repos/o/r/issues",
            "api repos/o/r/issues --method POST",
            "api repos/o/r -X GET -X DELETE",
            "api repos/o/r -X",
            "api -iXPOST repos/o/r",
            "api repos/o/r/issues -f title=x",
            "api repos/o/r/issues -ftitle=x",
            "api repos/o/r/issues -F n=1",
            "api repos/o/r/issues --field=title=x",
            "api repos/o/r/issues --raw-field title=x",
            "api repos/o/r/issues --input body.json",
            "api repos/o/r/issues -if title=x",
        ] {
            assert!(!rule.hold(&args(write)), "{write}");
        }
    }

    #[test]
    fn required_flags() {
        let rule = Conditions {
            require_flags: vec!["--draft".to_owned()],
            ..Conditions::default()
        };
        assert!(rule.hold(&args("pr create --draft --title t")));
        assert!(rule.hold(&args("pr create --title t --draft=true")));
        for missing in [
            "pr create --title t",
            "pr create --draft=false",
            "pr create --draft --draft=false",
            "pr create --drafty",
        ] {
            assert!(!rule.hold(&args(missing)), "{missing}");
        }
        let short = Conditions {
            require_flags: vec!["-d".to_owned()],
            ..Conditions::default()
        };
        assert!(short.hold(&args("pr create -d")));
        assert!(!short.hold(&args("pr create -wd")), "a cluster is unclear");
        assert!(!short.hold(&args("pr create -d=false")));
    }

    fn graphql() -> Conditions {
        Conditions {
            graphql: Some(GraphqlOperations::Query),
            ..Conditions::default()
        }
    }

    #[test]
    fn graphql_queries_pass_and_mutations_do_not() {
        let rule = graphql();
        for query in [
            "api graphql -f query='query { viewer { login } }'",
            "api graphql -f query='{ viewer { login } }'",
            "api graphql -fquery='{ viewer { login } }'",
            "api graphql --raw-field=query='query Q($n: String = \"mutation\") { repository(name: $n) { id } }'",
            "api graphql -F owner=o -f query='# mutation here is a comment\nquery { viewer { login } }'",
            "api graphql -f query='query A { a } fragment F on Repository { id }'",
            "api graphql -f query='{ a(text: \"\"\"mutation { x }\"\"\") }'",
        ] {
            assert!(rule.hold(&args(query)), "{query}");
        }
        for refused in [
            "api graphql -f query='mutation { addStar(input: {}) { clientMutationId } }'",
            "api graphql -f query='query A { a } mutation B { b }'",
            "api graphql -f query='subscription { s }'",
            "api graphql -f query='{ a } ' -f query='mutation { b }'",
            "api graphql -F query=@q.graphql",
            "api graphql -F query=@-",
            "api graphql --field=query=@q.graphql",
            "api graphql -f owner=o",
            "api graphql --input q.json -f query='{ a }'",
            "api graphql -f query='{ a '",
            "api graphql -f query='{ a } }'",
            "api graphql -f query='query'",
            "api graphql -f query='{ a(s: \"open) }'",
            "api graphql -f",
            "api graphql -if query='{ a }'",
        ] {
            assert!(!rule.hold(&args(refused)), "{refused}");
        }
    }

    #[test]
    fn documents() {
        assert!(document_is_query_only("{ a }"));
        assert!(document_is_query_only("query Mutation { a }"));
        assert!(!document_is_query_only("  mutation{a}"));
        assert!(!document_is_query_only(""));
        assert!(!document_is_query_only("{ a(s: \"\\\"\") } mutation { b }"));
    }

    #[test]
    fn problems() {
        assert!(get_only().problem().is_none());
        let bad = Conditions {
            forbid_flags: vec!["field".to_owned()],
            ..Conditions::default()
        };
        assert!(bad.problem().is_some());
        let empty = Conditions {
            flag_values: [("-X".to_owned(), Vec::new())].into(),
            ..Conditions::default()
        };
        assert!(empty.problem().is_some());
        assert!(Conditions::default().is_empty());
    }
}
