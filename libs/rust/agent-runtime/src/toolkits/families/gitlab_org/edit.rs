const MAX_EDIT_QUERY_BYTES: usize = 256 * 1_024;
const MAX_FILE_BYTES: usize = 1_024 * 1_024;
const MAX_EDIT_PAIRS: usize = 64;

const EDITABLE_EXTENSIONS: &[&str] = &[
    ".md", ".txt", ".csv", ".json", ".xml", ".html", ".yaml", ".yml", ".ini", ".conf", ".log",
    ".sh", ".py", ".js", ".ts", ".jsx", ".tsx", ".java", ".go", ".rb", ".php", ".c", ".cpp", ".h",
    ".hpp", ".cs", ".sql", ".r", ".m", ".swift", ".kt", ".rs", ".scala",
];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(in crate::toolkits) enum EditErrorCode {
    InvalidMarkers,
    UnsupportedFile,
    Ambiguous,
    NotFound,
    NoChange,
    ResourceExhausted,
}

#[derive(Clone, Copy)]
enum Section {
    None,
    Old,
    New,
}

pub(super) fn apply_update(
    file_path: &str,
    content: &str,
    query: &str,
) -> Result<String, EditErrorCode> {
    if query.len() > MAX_EDIT_QUERY_BYTES || content.len() > MAX_FILE_BYTES {
        return Err(EditErrorCode::ResourceExhausted);
    }
    if !editable(file_path) {
        return Err(EditErrorCode::UnsupportedFile);
    }
    let edits = parse_markers(query)?;
    let mut updated = content.to_owned();
    for (old, new) in edits {
        updated = apply_one(&updated, &old, &new)?;
        if updated.len() > MAX_FILE_BYTES {
            return Err(EditErrorCode::ResourceExhausted);
        }
    }
    if updated == content {
        return Err(EditErrorCode::NoChange);
    }
    Ok(updated)
}

fn editable(file_path: &str) -> bool {
    let lower = file_path.to_ascii_lowercase();
    EDITABLE_EXTENSIONS
        .iter()
        .any(|extension| lower.ends_with(extension))
}

fn parse_markers(query: &str) -> Result<Vec<(String, String)>, EditErrorCode> {
    let mut section = Section::None;
    let mut current = Vec::new();
    let mut old: Option<String> = None;
    let mut edits = Vec::new();

    for line in query.split('\n') {
        let marker = line.split_whitespace().collect::<String>();
        match marker.as_str() {
            "OLD<<<<" => {
                section = Section::Old;
                current.clear();
            }
            ">>>>OLD" => {
                if matches!(section, Section::Old) {
                    old = Some(current.join("\n").trim().to_owned());
                }
                section = Section::None;
                current.clear();
            }
            "NEW<<<<" => {
                section = Section::New;
                current.clear();
            }
            ">>>>NEW" => {
                if matches!(section, Section::New) {
                    let new = current.join("\n").trim().to_owned();
                    if let Some(old) = old.take() {
                        edits.push((old, new));
                        if edits.len() > MAX_EDIT_PAIRS {
                            return Err(EditErrorCode::ResourceExhausted);
                        }
                    }
                }
                section = Section::None;
                current.clear();
            }
            _ if matches!(section, Section::Old | Section::New) => {
                current.push(line.to_owned());
            }
            _ => {}
        }
    }
    if edits.is_empty() {
        Err(EditErrorCode::InvalidMarkers)
    } else {
        Ok(edits)
    }
}

fn apply_one(content: &str, old: &str, new: &str) -> Result<String, EditErrorCode> {
    if old.trim().is_empty() {
        return Err(EditErrorCode::NotFound);
    }
    let mut exact = content.match_indices(old).take(2);
    match (exact.next(), exact.next()) {
        (Some((start, _)), None) => {
            let mut updated = String::with_capacity(content.len() - old.len() + new.len());
            updated.push_str(&content[..start]);
            updated.push_str(new);
            updated.push_str(&content[start + old.len()..]);
            return Ok(updated);
        }
        (Some(_), Some(_)) => return Err(EditErrorCode::Ambiguous),
        (None, _) => {}
    }

    let old_lines = old
        .lines()
        .filter(|line| !line.trim().is_empty())
        .map(normalize_line)
        .collect::<Vec<_>>();
    if old_lines.is_empty() || content.is_empty() {
        return Err(EditErrorCode::NotFound);
    }
    let lines = line_ranges(content);
    let nonblank = lines
        .iter()
        .enumerate()
        .filter_map(|(line_index, &(start, end))| {
            let normalized = normalize_line(&content[start..end]);
            (!normalized.is_empty()).then_some((line_index, normalized))
        })
        .collect::<Vec<_>>();
    let prefix = kmp_prefix(&old_lines);
    let mut matched = 0usize;
    let mut candidate: Option<(usize, usize)> = None;
    let mut candidate_count = 0usize;
    for (nonblank_index, (line_index, normalized)) in nonblank.iter().enumerate() {
        while matched > 0 && old_lines[matched] != *normalized {
            matched = prefix[matched - 1];
        }
        if old_lines[matched] == *normalized {
            matched += 1;
        }
        if matched == old_lines.len() {
            let first_nonblank = nonblank_index + 1 - old_lines.len();
            let first_line = nonblank[first_nonblank].0;
            let previous_nonblank_line = first_nonblank
                .checked_sub(1)
                .map_or(0, |index| nonblank[index].0 + 1);
            let possible_starts = first_line - previous_nonblank_line + 1;
            candidate_count = candidate_count.saturating_add(possible_starts);
            if candidate_count > 1 {
                return Err(EditErrorCode::Ambiguous);
            }
            candidate = Some((lines[first_line].0, lines[*line_index].1));
            matched = prefix[matched - 1];
        }
    }
    let Some((start, end)) = candidate else {
        return Err(EditErrorCode::NotFound);
    };
    let suffix = &content[end..];
    let separator = if !suffix.is_empty() && !new.ends_with('\n') {
        "\n"
    } else {
        ""
    };
    let mut updated = String::with_capacity(content.len() - (end - start) + new.len() + 1);
    updated.push_str(&content[..start]);
    updated.push_str(new);
    updated.push_str(separator);
    updated.push_str(suffix);
    Ok(updated)
}

fn normalize_line(line: &str) -> String {
    let mut normalized = String::with_capacity(line.len());
    for token in line
        .replace(['\u{00a0}', '\u{2009}'], " ")
        .split_whitespace()
    {
        if !normalized.is_empty() {
            normalized.push(' ');
        }
        normalized.push_str(token);
    }
    normalized
}

fn kmp_prefix(pattern: &[String]) -> Vec<usize> {
    let mut prefix = vec![0; pattern.len()];
    let mut matched = 0;
    for index in 1..pattern.len() {
        while matched > 0 && pattern[index] != pattern[matched] {
            matched = prefix[matched - 1];
        }
        if pattern[index] == pattern[matched] {
            matched += 1;
            prefix[index] = matched;
        }
    }
    prefix
}

fn line_ranges(content: &str) -> Vec<(usize, usize)> {
    let mut ranges = Vec::new();
    let mut start = 0;
    for (index, character) in content.char_indices() {
        if character == '\n' {
            ranges.push((start, index + 1));
            start = index + 1;
        }
    }
    if start < content.len() {
        ranges.push((start, content.len()));
    }
    ranges
}

#[cfg(test)]
pub(in crate::toolkits) fn test_apply_update(
    file_path: &str,
    content: &str,
    query: &str,
) -> Result<String, EditErrorCode> {
    apply_update(file_path, content, query)
}
