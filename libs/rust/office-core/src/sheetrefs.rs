//! The sheet-reference grammar of spreadsheet formulas: a sheet name (quoted
//! with apostrophes, a doubled apostrophe for one inside the name, or bare)
//! followed by `!`, possibly a 3-D span `First:Last`. Used to find what refers
//! to a sheet and to rewrite it when the sheet is renamed.

/// One sheet prefix found in a text.
pub(crate) struct Prefix {
    /// Index (in chars) of the first char of the prefix, its opening quote
    /// included.
    pub(crate) start: usize,
    /// Index of the `!`.
    pub(crate) bang: usize,
    /// The sheet name or `First:Last` span, unquoted.
    pub(crate) token: String,
    /// Whether it was written in apostrophes.
    pub(crate) quoted: bool,
}

/// Every sheet prefix of `chars`, left to right.
pub(crate) fn prefixes(chars: &[char]) -> Vec<Prefix> {
    let mut out = Vec::new();
    for (i, &c) in chars.iter().enumerate() {
        if c != '!' || i == 0 {
            continue;
        }
        if chars[i - 1] == '\'' {
            let mut name = Vec::new();
            let mut p = i - 1;
            let mut closed = false;
            while p > 0 {
                p -= 1;
                if chars[p] == '\'' {
                    if p > 0 && chars[p - 1] == '\'' {
                        name.push('\'');
                        p -= 1;
                    } else {
                        closed = true;
                        break;
                    }
                } else {
                    name.push(chars[p]);
                }
            }
            if closed {
                out.push(Prefix {
                    start: p,
                    bang: i,
                    token: name.iter().rev().collect(),
                    quoted: true,
                });
            }
        } else {
            let mut p = i;
            while p > 0
                && (chars[p - 1].is_alphanumeric() || matches!(chars[p - 1], '_' | '.' | ':'))
            {
                p -= 1;
            }
            if p < i {
                out.push(Prefix {
                    start: p,
                    bang: i,
                    token: chars[p..i].iter().collect(),
                    quoted: false,
                });
            }
        }
    }
    out
}

/// Whether a name needs apostrophes to stand before a `!`.
fn needs_quotes(name: &str) -> bool {
    name.is_empty()
        || name.starts_with(|c: char| c.is_ascii_digit())
        || name
            .chars()
            .any(|c| !(c.is_alphanumeric() || matches!(c, '_' | '.' | ':')))
}

/// `text` with every reference to the sheet `old` (compared ignoring case, also
/// as an end of a 3-D span) written to `new`, quoted when the new name needs
/// it or the old reference was.
#[must_use]
pub fn rename_in_formula(text: &str, old: &str, new: &str) -> String {
    let chars: Vec<char> = text.chars().collect();
    let old = old.to_lowercase();
    let mut out = String::with_capacity(text.len() + 8);
    let mut at = 0usize;
    for p in prefixes(&chars) {
        if p.start < at {
            continue;
        }
        let parts: Vec<&str> = p.token.split(':').collect();
        if parts.len() > 2 || !parts.iter().any(|s| s.to_lowercase() == old) {
            continue;
        }
        let renamed: Vec<&str> = parts
            .iter()
            .map(|s| if s.to_lowercase() == old { new } else { *s })
            .collect();
        let joined = renamed.join(":");
        out.extend(&chars[at..p.start]);
        if p.quoted
            || renamed.iter().any(|s| needs_quotes(s))
            || needs_quotes(&joined.replace(':', ""))
        {
            out.push('\'');
            out.push_str(&joined.replace('\'', "''"));
            out.push('\'');
        } else {
            out.push_str(&joined);
        }
        at = p.bang;
    }
    out.extend(&chars[at..]);
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_rename_rewrites_every_spelling_and_quotes_when_needed() {
        assert_eq!(
            rename_in_formula("Old!$A$1:$B$2", "old", "New"),
            "New!$A$1:$B$2"
        );
        assert_eq!(
            rename_in_formula("'Old'!A1+'Old'!B1", "Old", "New"),
            "'New'!A1+'New'!B1"
        );
        assert_eq!(
            rename_in_formula("Old!A1", "Old", "My Sheet"),
            "'My Sheet'!A1"
        );
        assert_eq!(
            rename_in_formula("Old!A1", "Old", "O'Brien"),
            "'O''Brien'!A1"
        );
        assert_eq!(
            rename_in_formula("'O''Brien'!A1", "o'brien", "Plain"),
            "'Plain'!A1"
        );
        assert_eq!(
            rename_in_formula("SUM(Old:Last!A1)+Other!A1", "Old", "New"),
            "SUM(New:Last!A1)+Other!A1"
        );
        assert_eq!(rename_in_formula("Other!A1", "Old", "New"), "Other!A1");
        assert_eq!(rename_in_formula("A1+B2", "Old", "New"), "A1+B2");
        assert_eq!(rename_in_formula("Old!A1", "Old", "2024"), "'2024'!A1");
    }
}
