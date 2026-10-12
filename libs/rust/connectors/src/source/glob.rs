//! The SDK's attachment and file filters (`include_extensions`,
//! `skip_extensions`): each pattern is a Unix shell wildcard matched
//! case-insensitively against the file NAME; a bare extension (`pdf`,
//! `.pdf`) stands for `*.pdf` (`sharepoint/file_filters.py`,
//! `ado/wiki/ado_wrapper.py::_matches_any`). Skip patterns are evaluated
//! first; an empty include list admits every name.

/// Include and skip patterns, normalised.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct NameFilter {
    include: Vec<String>,
    skip: Vec<String>,
}

impl NameFilter {
    /// Patterns as the `index_data` arguments carry them.
    #[must_use]
    pub fn new<I, J, S, T>(include: I, skip: J) -> Self
    where
        I: IntoIterator<Item = S>,
        J: IntoIterator<Item = T>,
        S: AsRef<str>,
        T: AsRef<str>,
    {
        Self {
            include: include.into_iter().map(|p| normalise(p.as_ref())).collect(),
            skip: skip.into_iter().map(|p| normalise(p.as_ref())).collect(),
        }
    }

    /// Whether a file called `name` (its last path segment) passes.
    #[must_use]
    pub fn admits(&self, name: &str) -> bool {
        let name = name.rsplit('/').next().unwrap_or(name).to_lowercase();
        if self.skip.iter().any(|pattern| fnmatch(pattern, &name)) {
            return false;
        }
        self.include.is_empty() || self.include.iter().any(|pattern| fnmatch(pattern, &name))
    }

    /// Whether any pattern was given.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.include.is_empty() && self.skip.is_empty()
    }
}

/// `pdf`, `.pdf` and `*.pdf` become `*.pdf`; a pattern holding a wildcard or
/// a dot is used as it is (lower-cased).
fn normalise(pattern: &str) -> String {
    let trimmed = pattern.trim().to_lowercase();
    if trimmed.contains(['*', '?', '[']) || trimmed.contains('.') && !trimmed.starts_with('.') {
        return trimmed;
    }
    format!("*.{}", trimmed.trim_start_matches('.'))
}

/// Python's `fnmatch.fnmatchcase`: `*` any run, `?` one character, `[seq]`
/// one of, `[!seq]` none of; `/` is not special.
#[must_use]
pub fn fnmatch(pattern: &str, text: &str) -> bool {
    let pattern: Vec<char> = pattern.chars().collect();
    let text: Vec<char> = text.chars().collect();
    let (mut p, mut t) = (0, 0);
    let mut star: Option<(usize, usize)> = None;
    while t < text.len() {
        if p < pattern.len() {
            match pattern[p] {
                '*' => {
                    star = Some((p, t));
                    p += 1;
                    continue;
                }
                '?' => {
                    p += 1;
                    t += 1;
                    continue;
                }
                '[' => {
                    if let Some((matched, next)) = class(&pattern, p, text[t]) {
                        if matched {
                            p = next;
                            t += 1;
                            continue;
                        }
                    } else if text[t] == '[' {
                        p += 1;
                        t += 1;
                        continue;
                    }
                }
                literal if literal == text[t] => {
                    p += 1;
                    t += 1;
                    continue;
                }
                _ => {}
            }
        }
        match star {
            Some((star_p, star_t)) => {
                p = star_p + 1;
                t = star_t + 1;
                star = Some((star_p, star_t + 1));
            }
            None => return false,
        }
    }
    pattern[p..].iter().all(|c| *c == '*')
}

/// A bracket class at `start`: whether `character` is in it, and the index
/// after `]`. `None` when the bracket is not closed (then it is literal).
fn class(pattern: &[char], start: usize, character: char) -> Option<(bool, usize)> {
    let mut index = start + 1;
    let negated = index < pattern.len() && matches!(pattern[index], '!' | '^');
    if negated {
        index += 1;
    }
    let mut matched = false;
    let mut first = true;
    while index < pattern.len() {
        let current = pattern[index];
        if current == ']' && !first {
            return Some((matched != negated, index + 1));
        }
        first = false;
        if index + 2 < pattern.len() && pattern[index + 1] == '-' && pattern[index + 2] != ']' {
            let (low, high) = (current, pattern[index + 2]);
            if low <= character && character <= high {
                matched = true;
            }
            index += 3;
        } else {
            if current == character {
                matched = true;
            }
            index += 1;
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bare_extensions_become_globs_and_skip_wins_over_include() {
        let filter = NameFilter::new(["pdf", ".Docx", "*.md"], ["draft*"]);
        assert!(filter.admits("Spec.PDF"));
        assert!(filter.admits("a/b/notes.docx"));
        assert!(filter.admits("README.md"));
        assert!(!filter.admits("draft.pdf"));
        assert!(!filter.admits("image.png"));
        assert!(NameFilter::default().admits("anything"));
        assert!(!NameFilter::new(Vec::<&str>::new(), ["png"]).admits("x.PNG"));
        // A pattern with a dot is a file name, not an extension.
        assert!(NameFilter::new(["readme.md"], Vec::<&str>::new()).admits("README.md"));
        assert!(!NameFilter::new(["readme.md"], Vec::<&str>::new()).admits("other.md"));
    }

    #[test]
    fn fnmatch_follows_python() {
        assert!(fnmatch("*.py", "src/app.py"));
        assert!(fnmatch("*test_*", "tests/test_app.py"));
        assert!(fnmatch("src/?.rs", "src/a.rs"));
        assert!(fnmatch("[a-c].md", "c.md"));
        assert!(fnmatch("docs/**", "docs/a/b.md"));
        assert!(fnmatch("*", ""));
        assert!(fnmatch("README", "README"));
        assert!(!fnmatch("readme", "README"), "case-sensitive as given");
        assert!(!fnmatch("src/?.rs", "src/ab.rs"));
        assert!(fnmatch("[abc].md", "b.md"));
        assert!(!fnmatch("[!abc].md", "b.md"));
        assert!(fnmatch("a[", "a["));
        assert!(!fnmatch("?", ""));
    }
}
