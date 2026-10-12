//! `tools/ado/utils.py::generate_diff`: Python's `difflib.unified_diff` over
//! `splitlines(keepends=True)`, reproduced exactly: the `SequenceMatcher`
//! longest-match recursion with its autojunk "popular line" rule, grouped
//! opcodes with three lines of context, and the unified hunk format.

use std::collections::HashMap;
use std::fmt::Write as _;

use crate::toolkits::families::ado::format::python_lines;
use crate::toolkits::families::vcs_text::python_line_ranges;

const CONTEXT: usize = 3;
/// One file the diff reads; larger files are reported, not diffed.
pub(crate) const MAX_DIFF_FILE_BYTES: usize = 1_024 * 1_024;
/// The longest side, in lines, one file's diff compares. `SequenceMatcher`
/// is O(n·m) in lines in the worst case, so the byte cap alone still admits a
/// 1 MiB file of short lines — hundreds of thousands per side. Above this a
/// file is reported, not diffed, and the rest of the pull request still is.
pub(crate) const MAX_DIFF_FILE_LINES: usize = 10_000;

/// One `edit` entry's diff text under both caps: the unified diff, or a note
/// naming why it was omitted. Never an error — one oversized file must not
/// fail the whole `list_pull_request_files` call. CPU-bound: callers run it
/// on the blocking pool.
pub(crate) fn bounded_file_diff(
    change_type: &str,
    base: &str,
    target: &str,
    file_path: &str,
) -> String {
    if base.len() > MAX_DIFF_FILE_BYTES || target.len() > MAX_DIFF_FILE_BYTES {
        return format!("Change Type: {change_type} (file too large to diff)");
    }
    let lines = python_line_ranges(base)
        .len()
        .max(python_line_ranges(target).len());
    if lines > MAX_DIFF_FILE_LINES {
        return format!(
            "Change Type: {change_type} (diff omitted: file too large ({lines} lines))"
        );
    }
    generate_diff(base, target, file_path)
}

/// The unified diff of `base` → `target` as `a/<path>` → `b/<path>`; empty
/// when the two are equal.
pub(crate) fn generate_diff(base: &str, target: &str, file_path: &str) -> String {
    let a = python_lines(base);
    let b = python_lines(target);
    let groups = grouped_opcodes(&a, &b, CONTEXT);
    let mut output = String::new();
    for (index, group) in groups.iter().enumerate() {
        if index == 0 {
            let _ = write!(output, "--- a/{file_path}\n+++ b/{file_path}\n");
        }
        let (first, last) = (group[0], group[group.len() - 1]);
        let _ = writeln!(
            output,
            "@@ -{} +{} @@",
            format_range(first.1, last.2),
            format_range(first.3, last.4)
        );
        for &(tag, i1, i2, j1, j2) in group {
            if tag == Tag::Equal {
                for line in &a[i1..i2] {
                    output.push(' ');
                    output.push_str(line);
                }
                continue;
            }
            if matches!(tag, Tag::Replace | Tag::Delete) {
                for line in &a[i1..i2] {
                    output.push('-');
                    output.push_str(line);
                }
            }
            if matches!(tag, Tag::Replace | Tag::Insert) {
                for line in &b[j1..j2] {
                    output.push('+');
                    output.push_str(line);
                }
            }
        }
    }
    output
}

fn format_range(start: usize, stop: usize) -> String {
    let mut beginning = start + 1;
    let length = stop - start;
    if length == 1 {
        return beginning.to_string();
    }
    if length == 0 {
        beginning -= 1;
    }
    format!("{beginning},{length}")
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Tag {
    Replace,
    Delete,
    Insert,
    Equal,
}

type Opcode = (Tag, usize, usize, usize, usize);

struct Matcher<'a> {
    a: &'a [&'a str],
    b: &'a [&'a str],
    b2j: HashMap<&'a str, Vec<usize>>,
}

impl<'a> Matcher<'a> {
    fn new(a: &'a [&'a str], b: &'a [&'a str]) -> Self {
        let mut b2j: HashMap<&str, Vec<usize>> = HashMap::new();
        for (index, line) in b.iter().enumerate() {
            b2j.entry(line).or_default().push(index);
        }
        // autojunk: in a sequence of 200+ lines, a line seen more than 1% of
        // the time is "popular" and never seeds a match.
        if b.len() >= 200 {
            let threshold = b.len() / 100 + 1;
            b2j.retain(|_, indices| indices.len() <= threshold);
        }
        Self { a, b, b2j }
    }

    fn find_longest_match(
        &self,
        a_low: usize,
        a_high: usize,
        b_low: usize,
        b_high: usize,
    ) -> (usize, usize, usize) {
        let (mut best_i, mut best_j, mut best_size) = (a_low, b_low, 0usize);
        let mut lengths: HashMap<usize, usize> = HashMap::new();
        for i in a_low..a_high {
            let mut next: HashMap<usize, usize> = HashMap::new();
            if let Some(indices) = self.b2j.get(self.a[i]) {
                for &j in indices {
                    if j < b_low {
                        continue;
                    }
                    if j >= b_high {
                        break;
                    }
                    let k = j
                        .checked_sub(1)
                        .and_then(|previous| lengths.get(&previous))
                        .copied()
                        .unwrap_or(0)
                        + 1;
                    next.insert(j, k);
                    if k > best_size {
                        best_i = i + 1 - k;
                        best_j = j + 1 - k;
                        best_size = k;
                    }
                }
            }
            lengths = next;
        }
        // No line is junk (no `isjunk`), so only the first two extension
        // passes of difflib can move: they grow the match over equal lines,
        // popular ones included.
        while best_i > a_low && best_j > b_low && self.a[best_i - 1] == self.b[best_j - 1] {
            best_i -= 1;
            best_j -= 1;
            best_size += 1;
        }
        while best_i + best_size < a_high
            && best_j + best_size < b_high
            && self.a[best_i + best_size] == self.b[best_j + best_size]
        {
            best_size += 1;
        }
        (best_i, best_j, best_size)
    }

    fn matching_blocks(&self) -> Vec<(usize, usize, usize)> {
        let (la, lb) = (self.a.len(), self.b.len());
        let mut queue = vec![(0, la, 0, lb)];
        let mut blocks = Vec::new();
        while let Some((a_low, a_high, b_low, b_high)) = queue.pop() {
            let (i, j, k) = self.find_longest_match(a_low, a_high, b_low, b_high);
            if k > 0 {
                blocks.push((i, j, k));
                if a_low < i && b_low < j {
                    queue.push((a_low, i, b_low, j));
                }
                if i + k < a_high && j + k < b_high {
                    queue.push((i + k, a_high, j + k, b_high));
                }
            }
        }
        blocks.sort_unstable();
        let (mut i1, mut j1, mut k1) = (0, 0, 0);
        let mut collapsed = Vec::with_capacity(blocks.len() + 1);
        for (i2, j2, k2) in blocks {
            if i1 + k1 == i2 && j1 + k1 == j2 {
                k1 += k2;
            } else {
                if k1 > 0 {
                    collapsed.push((i1, j1, k1));
                }
                (i1, j1, k1) = (i2, j2, k2);
            }
        }
        if k1 > 0 {
            collapsed.push((i1, j1, k1));
        }
        collapsed.push((la, lb, 0));
        collapsed
    }

    fn opcodes(&self) -> Vec<Opcode> {
        let (mut i, mut j) = (0, 0);
        let mut answer = Vec::new();
        for (ai, bj, size) in self.matching_blocks() {
            let tag = if i < ai && j < bj {
                Some(Tag::Replace)
            } else if i < ai {
                Some(Tag::Delete)
            } else if j < bj {
                Some(Tag::Insert)
            } else {
                None
            };
            if let Some(tag) = tag {
                answer.push((tag, i, ai, j, bj));
            }
            (i, j) = (ai + size, bj + size);
            if size > 0 {
                answer.push((Tag::Equal, ai, i, bj, j));
            }
        }
        answer
    }
}

fn grouped_opcodes(a: &[&str], b: &[&str], context: usize) -> Vec<Vec<Opcode>> {
    let matcher = Matcher::new(a, b);
    let mut codes = matcher.opcodes();
    if codes.is_empty() {
        codes.push((Tag::Equal, 0, 1, 0, 1));
    }
    if let Some(first) = codes.first_mut()
        && first.0 == Tag::Equal
    {
        let (tag, i1, i2, j1, j2) = *first;
        *first = (
            tag,
            i1.max(i2.saturating_sub(context)),
            i2,
            j1.max(j2.saturating_sub(context)),
            j2,
        );
    }
    if let Some(last) = codes.last_mut()
        && last.0 == Tag::Equal
    {
        let (tag, i1, i2, j1, j2) = *last;
        *last = (tag, i1, i2.min(i1 + context), j1, j2.min(j1 + context));
    }
    let double = context * 2;
    let mut groups = Vec::new();
    let mut group = Vec::new();
    for (tag, mut i1, i2, mut j1, j2) in codes {
        if tag == Tag::Equal && i2 - i1 > double {
            group.push((tag, i1, i2.min(i1 + context), j1, j2.min(j1 + context)));
            groups.push(std::mem::take(&mut group));
            i1 = i1.max(i2.saturating_sub(context));
            j1 = j1.max(j2.saturating_sub(context));
        }
        group.push((tag, i1, i2, j1, j2));
    }
    let only_equal = group.len() == 1 && group[0].0 == Tag::Equal;
    if !group.is_empty() && !only_equal {
        groups.push(group);
    }
    groups
}
