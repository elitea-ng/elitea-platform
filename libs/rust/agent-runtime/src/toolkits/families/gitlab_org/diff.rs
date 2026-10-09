use serde_json::{Map, Value};

const MAX_DIFF_BYTES: usize = 512 * 1_024;
const MAX_CHANGES: usize = 100;
const MAX_PATH_BYTES: usize = 1_024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(in crate::toolkits) enum DiffErrorCode {
    InvalidShape,
    InvalidIndex,
    ResourceExhausted,
}

#[derive(Clone, Debug, Default)]
struct Position<'a> {
    old_line: Option<u64>,
    old_path: Option<&'a str>,
    new_line: Option<u64>,
    new_path: Option<&'a str>,
}

#[derive(Clone, Debug)]
struct DiffRow<'a> {
    index: usize,
    text: &'a str,
    position: Option<Position<'a>>,
}

pub(super) fn format_changes(
    merge_request: &Value,
    changes_response: &Value,
) -> Result<String, DiffErrorCode> {
    let title = required_string(merge_request, "title")?;
    let description = match merge_request.get("description") {
        None | Some(Value::Null) => "None",
        Some(Value::String(value)) => value,
        Some(_) => return Err(DiffErrorCode::InvalidShape),
    };
    let changes = changes(changes_response)?;
    let mut output = String::new();
    push_bounded(&mut output, "title: ")?;
    push_bounded(&mut output, title)?;
    push_bounded(&mut output, "\ndescription: ")?;
    push_bounded(&mut output, description)?;
    push_bounded(&mut output, "\n\n")?;
    for change in changes {
        let old_path = required_string(change, "old_path")?;
        let new_path = required_string(change, "new_path")?;
        let diff = required_string(change, "diff")?;
        if old_path.len() > MAX_PATH_BYTES
            || new_path.len() > MAX_PATH_BYTES
            || diff.len() > MAX_DIFF_BYTES
        {
            return Err(DiffErrorCode::ResourceExhausted);
        }
        push_bounded(&mut output, "diff --git a/")?;
        push_bounded(&mut output, old_path)?;
        push_bounded(&mut output, " b/")?;
        push_bounded(&mut output, new_path)?;
        push_bounded(&mut output, "\n")?;
        for row in parse_rows(diff, old_path, new_path)? {
            push_bounded(&mut output, &row.index.to_string())?;
            push_bounded(&mut output, ":")?;
            push_bounded(&mut output, row.text)?;
            push_bounded(&mut output, "\n")?;
        }
    }
    Ok(output)
}

pub(super) fn discussion_position(
    merge_request: &Value,
    changes_response: &Value,
    file_path: &str,
    line_number: usize,
) -> Result<Map<String, Value>, DiffErrorCode> {
    let changes = changes(changes_response)?;
    let change = changes
        .iter()
        .copied()
        .find(|change| change.get("new_path").and_then(Value::as_str) == Some(file_path))
        .or_else(|| {
            changes
                .iter()
                .copied()
                .find(|change| change.get("old_path").and_then(Value::as_str) == Some(file_path))
        })
        .ok_or(DiffErrorCode::InvalidIndex)?;
    let old_path = required_string(change, "old_path")?;
    let new_path = required_string(change, "new_path")?;
    let diff = required_string(change, "diff")?;
    if old_path.len() > MAX_PATH_BYTES
        || new_path.len() > MAX_PATH_BYTES
        || diff.len() > MAX_DIFF_BYTES
    {
        return Err(DiffErrorCode::ResourceExhausted);
    }
    let row = parse_rows(diff, old_path, new_path)?
        .into_iter()
        .find(|row| row.index == line_number)
        .ok_or(DiffErrorCode::InvalidIndex)?;
    let position = row.position.ok_or(DiffErrorCode::InvalidIndex)?;
    let refs = merge_request
        .get("diff_refs")
        .and_then(Value::as_object)
        .ok_or(DiffErrorCode::InvalidShape)?;
    let mut result = Map::new();
    if let Some(value) = position.old_line {
        result.insert("old_line".to_owned(), Value::from(value));
    }
    if let Some(value) = position.old_path {
        result.insert("old_path".to_owned(), Value::String(value.to_owned()));
    }
    if let Some(value) = position.new_line {
        result.insert("new_line".to_owned(), Value::from(value));
    }
    if let Some(value) = position.new_path {
        result.insert("new_path".to_owned(), Value::String(value.to_owned()));
    }
    for name in ["base_sha", "head_sha", "start_sha"] {
        result.insert(
            name.to_owned(),
            Value::String(
                refs.get(name)
                    .and_then(Value::as_str)
                    .filter(|value| !value.is_empty())
                    .ok_or(DiffErrorCode::InvalidShape)?
                    .to_owned(),
            ),
        );
    }
    result.insert("position_type".to_owned(), Value::String("text".to_owned()));
    Ok(result)
}

fn changes(response: &Value) -> Result<Vec<&Value>, DiffErrorCode> {
    let changes = response
        .get("changes")
        .and_then(Value::as_array)
        .ok_or(DiffErrorCode::InvalidShape)?;
    if changes.len() > MAX_CHANGES {
        return Err(DiffErrorCode::ResourceExhausted);
    }
    Ok(changes.iter().collect())
}

fn parse_rows<'a>(
    diff: &'a str,
    old_path: &'a str,
    new_path: &'a str,
) -> Result<Vec<DiffRow<'a>>, DiffErrorCode> {
    let mut rows: Vec<DiffRow<'_>> = Vec::new();
    let mut old_line = None;
    let mut new_line = None;
    let mut lines = diff.split('\n').peekable();
    let mut index = 0usize;
    while let Some(line) = lines.next() {
        if line.starts_with("@@ ") {
            let (old, new) = parse_hunk_header(line)?;
            old_line = Some(old);
            new_line = Some(new);
            rows.push(DiffRow {
                index,
                text: line,
                position: None,
            });
            index = index
                .checked_add(1)
                .ok_or(DiffErrorCode::ResourceExhausted)?;
            continue;
        }
        if line.is_empty() && lines.peek().is_none() {
            break;
        }
        let position = match line.as_bytes().first().copied() {
            Some(b'+') => {
                let line_number = new_line.ok_or(DiffErrorCode::InvalidShape)?;
                new_line = Some(
                    line_number
                        .checked_add(1)
                        .ok_or(DiffErrorCode::ResourceExhausted)?,
                );
                Position {
                    new_line: Some(line_number),
                    new_path: Some(new_path),
                    ..Position::default()
                }
            }
            Some(b'-') => {
                let line_number = old_line.ok_or(DiffErrorCode::InvalidShape)?;
                old_line = Some(
                    line_number
                        .checked_add(1)
                        .ok_or(DiffErrorCode::ResourceExhausted)?,
                );
                Position {
                    old_line: Some(line_number),
                    old_path: Some(old_path),
                    ..Position::default()
                }
            }
            Some(b' ') => {
                let old_number = old_line.ok_or(DiffErrorCode::InvalidShape)?;
                let new_number = new_line.ok_or(DiffErrorCode::InvalidShape)?;
                old_line = Some(
                    old_number
                        .checked_add(1)
                        .ok_or(DiffErrorCode::ResourceExhausted)?,
                );
                new_line = Some(
                    new_number
                        .checked_add(1)
                        .ok_or(DiffErrorCode::ResourceExhausted)?,
                );
                Position {
                    old_line: Some(old_number),
                    old_path: Some(old_path),
                    new_line: Some(new_number),
                    new_path: Some(new_path),
                }
            }
            Some(b'\\') => rows
                .last()
                .and_then(|row| row.position.clone())
                .ok_or(DiffErrorCode::InvalidShape)?,
            _ => return Err(DiffErrorCode::InvalidShape),
        };
        if let Some(header) = rows
            .last_mut()
            .filter(|row| row.text.starts_with("@@ ") && row.position.is_none())
        {
            header.position = Some(position.clone());
        }
        rows.push(DiffRow {
            index,
            text: line,
            position: Some(position),
        });
        index = index
            .checked_add(1)
            .ok_or(DiffErrorCode::ResourceExhausted)?;
    }
    Ok(rows)
}

fn push_bounded(output: &mut String, value: &str) -> Result<(), DiffErrorCode> {
    if output
        .len()
        .checked_add(value.len())
        .is_none_or(|length| length > MAX_DIFF_BYTES)
    {
        return Err(DiffErrorCode::ResourceExhausted);
    }
    output.push_str(value);
    Ok(())
}

fn parse_hunk_header(line: &str) -> Result<(u64, u64), DiffErrorCode> {
    let remainder = line
        .strip_prefix("@@ -")
        .ok_or(DiffErrorCode::InvalidShape)?;
    let (ranges, _) = remainder
        .split_once(" @@")
        .ok_or(DiffErrorCode::InvalidShape)?;
    let mut ranges = ranges.split_whitespace();
    let old = parse_range(ranges.next().ok_or(DiffErrorCode::InvalidShape)?)?;
    let new = ranges
        .next()
        .and_then(|value| value.strip_prefix('+'))
        .ok_or(DiffErrorCode::InvalidShape)
        .and_then(parse_range)?;
    Ok((old, new))
}

fn parse_range(value: &str) -> Result<u64, DiffErrorCode> {
    value
        .split(',')
        .next()
        .and_then(|value| value.parse().ok())
        .ok_or(DiffErrorCode::InvalidShape)
}

fn required_string<'a>(value: &'a Value, name: &str) -> Result<&'a str, DiffErrorCode> {
    value
        .get(name)
        .and_then(Value::as_str)
        .ok_or(DiffErrorCode::InvalidShape)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_format_changes(
    merge_request: &Value,
    changes_response: &Value,
) -> Result<String, DiffErrorCode> {
    format_changes(merge_request, changes_response)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_discussion_position(
    merge_request: &Value,
    changes_response: &Value,
    file_path: &str,
    line_number: usize,
) -> Result<Map<String, Value>, DiffErrorCode> {
    discussion_position(merge_request, changes_response, file_path, line_number)
}
