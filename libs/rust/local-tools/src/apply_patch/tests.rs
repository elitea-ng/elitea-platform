// Portions adapted from OpenAI Codex (https://github.com/openai/codex), the unit tests of codex-rs/apply-patch/src/seek_sequence.rs and codex-rs/apply-patch/src/lib.rs @ be9060f. Apache License 2.0. Copyright 2025 OpenAI.
// Changes: run against this crate's pure functions and confined Workspace;
// the pass and uniqueness are asserted; security regressions added.

use serde_json::json;

use super::seek_sequence::{Pass, SeekError, seek_sequence};
use super::{derive_new_contents, parse_patch};
use crate::error::ErrorCode;
use crate::files::{apply_patch, plan_patch, read};
use crate::ledger::ReadLedger;
use crate::workspace::Workspace;

fn to_vec(strings: &[&str]) -> Vec<String> {
    strings.iter().map(ToString::to_string).collect()
}

#[test]
fn seek_sequence_exact_match_finds_sequence() {
    let lines = to_vec(&["foo", "bar", "baz"]);
    let pattern = to_vec(&["bar", "baz"]);
    assert_eq!(
        seek_sequence(&lines, &pattern, 0, false),
        Ok((1, Pass::Exact))
    );
}

#[test]
fn seek_sequence_trim_end_ignores_trailing_whitespace() {
    let lines = to_vec(&["foo   ", "bar\t\t"]);
    let pattern = to_vec(&["foo", "bar"]);
    assert_eq!(
        seek_sequence(&lines, &pattern, 0, false),
        Ok((0, Pass::TrimEnd))
    );
}

#[test]
fn seek_sequence_trim_ignores_leading_and_trailing_whitespace() {
    let lines = to_vec(&["    foo   ", "   bar\t"]);
    let pattern = to_vec(&["foo", "bar"]);
    assert_eq!(
        seek_sequence(&lines, &pattern, 0, false),
        Ok((0, Pass::Trim))
    );
}

#[test]
fn seek_sequence_unicode_normalises_punctuation() {
    let lines = to_vec(&[
        "let s = \u{201C}a \u{2014} b\u{201D};",
        "x\u{00A0}= \u{2018}y\u{2019}",
    ]);
    let pattern = to_vec(&["let s = \"a - b\";", "x = 'y'"]);
    assert_eq!(
        seek_sequence(&lines, &pattern, 0, false),
        Ok((0, Pass::Unicode))
    );
}

#[test]
fn seek_sequence_eof_anchored_prefers_the_end() {
    let lines = to_vec(&["end", "middle", "end"]);
    let pattern = to_vec(&["end"]);
    assert_eq!(
        seek_sequence(&lines, &pattern, 0, true),
        Ok((2, Pass::Exact))
    );
    assert_eq!(
        seek_sequence(&lines, &pattern, 0, false),
        Ok((0, Pass::Exact))
    );
    // Not at the end: found from `start` instead.
    let lines = to_vec(&["end", "tail"]);
    assert_eq!(
        seek_sequence(&lines, &pattern, 0, true),
        Ok((0, Pass::Exact))
    );
}

#[test]
fn seek_sequence_pattern_longer_than_input_returns_none() {
    let lines = to_vec(&["just one line"]);
    let pattern = to_vec(&["too", "many", "lines"]);
    assert_eq!(
        seek_sequence(&lines, &pattern, 0, false),
        Err(SeekError::NotFound)
    );
}

#[test]
fn seek_sequence_ambiguous_fuzzy_lists_candidates() {
    let lines = to_vec(&["a  ", "b", "a\t", "b"]);
    let pattern = to_vec(&["a", "b"]);
    assert_eq!(
        seek_sequence(&lines, &pattern, 0, false),
        Err(SeekError::Ambiguous {
            pass: Pass::TrimEnd,
            candidates: vec![0, 2]
        })
    );
    // Several exact matches: the first, as patches apply in order.
    let lines = to_vec(&["a", "b", "a", "b"]);
    assert_eq!(
        seek_sequence(&lines, &pattern, 1, false),
        Ok((2, Pass::Exact))
    );
}

fn update(original: &str, body: &str) -> crate::error::ToolResult<super::Derived> {
    let text = format!("*** Begin Patch\n*** Update File: f.txt\n{body}\n*** End Patch");
    let hunks = parse_patch(&text)?;
    let Some(super::Hunk::UpdateFile { chunks, .. }) = hunks.first() else {
        panic!("an update");
    };
    derive_new_contents(original, "f.txt", chunks)
}

#[test]
fn multiple_chunks_apply_to_one_file() {
    let derived = update(
        "foo\nbar\nbaz\nqux\n",
        "@@\n foo\n-bar\n+BAR\n@@\n baz\n-qux\n+QUX",
    )
    .expect("apply");
    assert_eq!(derived.content, "foo\nBAR\nbaz\nQUX\n");
    assert_eq!(derived.matches, [Pass::Exact, Pass::Exact]);
}

#[test]
fn interleaved_changes_and_end_of_file() {
    let derived = update(
        "a\nb\nc\nd\ne\nf\n",
        "@@\n a\n-b\n+B\n@@\n c\n d\n-e\n+E\n@@\n f\n+g\n*** End of File",
    )
    .expect("apply");
    assert_eq!(derived.content, "a\nB\nc\nd\nE\nf\ng\n");
}

#[test]
fn pure_addition_chunk_followed_by_removal() {
    let derived = update(
        "line1\nline2\nline3\n",
        "@@\n+after-context\n+second-line\n@@\n line1\n-line2\n-line3\n+line2-replacement",
    )
    .expect("apply");
    assert_eq!(
        derived.content,
        "line1\nline2-replacement\nafter-context\nsecond-line\n"
    );
}

#[test]
fn unicode_dash_is_matched_and_reported() {
    let derived = update(
        "import asyncio  # local import \u{2013} avoids top\u{2011}level dep\n",
        "@@\n-import asyncio  # local import - avoids top-level dep\n+import asyncio  # HELLO",
    )
    .expect("apply");
    assert_eq!(derived.content, "import asyncio  # HELLO\n");
    assert_eq!(derived.matches, [Pass::Unicode]);
}

#[test]
fn anchors_narrow_and_crlf_is_kept() {
    let derived = update(
        "fn a() {\r\n    x\r\n}\r\nfn b() {\r\n    x\r\n}\r\n",
        "@@ fn b() {\n-    x\n+    y",
    )
    .expect("apply");
    assert_eq!(
        derived.content,
        "fn a() {\r\n    x\r\n}\r\nfn b() {\r\n    y\r\n}\r\n"
    );
    assert_eq!(derived.changed, [(4, 5)]);
}

/// An anchored pure addition lands right after the anchor line, not at
/// the end of the file; an anchorless one still appends (Codex's rule).
#[test]
fn anchored_pure_addition_goes_after_its_anchor() {
    let original = "def foo():\n    return 1\n\ndef bar():\n    return 2\n";
    let derived = update(original, "@@ def foo():\n+    log()").expect("apply");
    assert_eq!(
        derived.content,
        "def foo():\n    log()\n    return 1\n\ndef bar():\n    return 2\n"
    );
    assert_eq!(derived.matches, [Pass::Exact]);
    assert_eq!(derived.changed, [(1, 2)]);
    // The anchor matched fuzzily: the pass is reported, the place kept.
    let derived =
        update("def foo():  \n    return 1\n", "@@ def foo():\n+    log()").expect("apply");
    assert_eq!(derived.content, "def foo():  \n    log()\n    return 1\n");
    assert_eq!(derived.matches, [Pass::TrimEnd]);
    // No anchor: appended at the end.
    let derived = update(original, "@@\n+# tail").expect("apply");
    assert_eq!(derived.content, format!("{original}# tail\n"));
}

/// A file without a final newline keeps it so when the patch leaves its
/// last line alone, and gains one when the patch changes or extends it.
#[test]
fn missing_final_newline_survives_an_unrelated_change() {
    let derived = update("a\nb\nc", "@@\n-a\n+A").expect("apply");
    assert_eq!(derived.content, "A\nb\nc");
    let derived = update("a\nb\nc", "@@\n b\n-c\n+C").expect("apply");
    assert_eq!(derived.content, "a\nb\nC\n");
    let derived = update("a\nb\nc", "@@\n+d").expect("apply");
    assert_eq!(derived.content, "a\nb\nc\nd\n");
    // Context ending at the unterminated last line leaves it unterminated.
    let derived = update("a\nb\nc", "@@\n-a\n+A\n b\n c").expect("apply");
    assert_eq!(derived.content, "A\nb\nc");
}

/// Only `\n` and `\r\n` end a line: a bare CR inside a line is content,
/// so a chunk naming that line (CR and all) matches it and the CR stays.
#[test]
fn bare_cr_is_line_content() {
    let derived = update("a\rb\nc\n", "@@\n a\rb\n-c\n+C").expect("apply");
    assert_eq!(derived.content, "a\rb\nC\n");
    assert_eq!(derived.changed, [(1, 2)]);
}

/// Inserted lines take the file's majority ending, not its first one.
#[test]
fn inserted_lines_take_the_majority_ending() {
    let derived = update("a\nb\r\nc\r\nd\r\n", "@@\n+e").expect("apply");
    assert_eq!(derived.content, "a\nb\r\nc\r\nd\r\ne\r\n");
}

fn setup(deny: &[&str]) -> (tempfile::TempDir, Workspace, ReadLedger) {
    let dir = tempfile::tempdir().expect("dir");
    let deny: Vec<String> = deny.iter().map(|s| (*s).to_owned()).collect();
    let workspace = Workspace::open(dir.path(), &deny).expect("workspace");
    (dir, workspace, ReadLedger::new())
}

fn seed(dir: &tempfile::TempDir, name: &str, content: &str) {
    let path = dir.path().join(name);
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent).expect("dirs");
    }
    std::fs::write(path, content).expect("seed");
}

fn read_all(ws: &Workspace, ledger: &ReadLedger, names: &[&str]) {
    for name in names {
        read(ws, ledger, json!({ "path": name }), 1 << 20, false).expect("read");
    }
}

fn content(dir: &tempfile::TempDir, name: &str) -> String {
    std::fs::read_to_string(dir.path().join(name)).expect(name)
}

#[test]
fn codex_envelope_add_update_delete_move() {
    let (dir, ws, ledger) = setup(&[]);
    seed(
        &dir,
        "src/app.py",
        "def f():\n    pass\n\ndef g():\n    return 1  \n",
    );
    seed(&dir, "old.txt", "bye\n");
    seed(&dir, "move/from.txt", "one\ntwo\n");
    read_all(&ws, &ledger, &["src/app.py", "old.txt", "move/from.txt"]);
    let patch = "*** Begin Patch\n*** Add File: docs/new.md\n+# New\n+text\n*** Delete File: old.txt\n*** Update File: src/app.py\n@@ def f():\n-    pass\n+    return 0\n@@ def g():\n-    return 1\n+    return 2\n*** Update File: move/from.txt\n*** Move to: moved/to.txt\n@@\n one\n-two\n+TWO\n*** End Patch";
    let planned = plan_patch(&ws, &ledger, patch).expect("plan");
    let result = apply_patch(&ws, &ledger, &planned).expect("apply");
    assert_eq!(
        result["changed"],
        json!([
            "docs/new.md",
            "old.txt",
            "src/app.py",
            "moved/to.txt",
            "move/from.txt"
        ])
    );
    assert_eq!(content(&dir, "docs/new.md"), "# New\ntext\n");
    assert!(!dir.path().join("old.txt").exists());
    assert_eq!(
        content(&dir, "src/app.py"),
        "def f():\n    return 0\n\ndef g():\n    return 2\n"
    );
    assert_eq!(content(&dir, "moved/to.txt"), "one\nTWO\n");
    assert!(!dir.path().join("move/from.txt").exists());
    // The trailing whitespace on `return 1  ` matched on the second pass,
    // and the result says so.
    let files = result["files"].as_array().expect("files");
    let app = files
        .iter()
        .find(|file| file["path"] == "src/app.py")
        .expect("app");
    assert_eq!(app["matches"], json!(["exact", "trim_end"]));
    assert!(result["note"].as_str().expect("note").contains("1 hunk"));
}

#[test]
fn codex_envelope_add_update_delete_move_is_atomic_on_failure() {
    let (dir, ws, ledger) = setup(&[]);
    seed(&dir, "a.txt", "1\n2\n3\n");
    seed(&dir, "b.txt", "x\n");
    read_all(&ws, &ledger, &["a.txt", "b.txt"]);
    // The second update's lines are not in b.txt: nothing is written, the
    // added file included.
    let patch = "*** Begin Patch\n*** Add File: fresh.txt\n+fresh\n*** Update File: a.txt\n@@\n 1\n-2\n+two\n*** Update File: b.txt\n@@\n-nope\n+y\n*** End Patch";
    let error = plan_patch(&ws, &ledger, patch).err().expect("conflict");
    assert_eq!(error.code(), ErrorCode::Conflict);
    assert_eq!(content(&dir, "a.txt"), "1\n2\n3\n");
    assert_eq!(content(&dir, "b.txt"), "x\n");
    assert!(!dir.path().join("fresh.txt").exists());
    // An unread file refuses the envelope before anything is written.
    seed(&dir, "unread.txt", "u\n");
    let patch = "*** Begin Patch\n*** Update File: a.txt\n@@\n-1\n+one\n*** Delete File: unread.txt\n*** End Patch";
    assert_eq!(
        plan_patch(&ws, &ledger, patch).err().map(|e| e.code()),
        Some(ErrorCode::StaleRead)
    );
    // A file changed since it was read is refused.
    std::fs::write(dir.path().join("a.txt"), "changed\n").expect("external");
    let patch = "*** Begin Patch\n*** Update File: a.txt\n@@\n-changed\n+mine\n*** End Patch";
    assert_eq!(
        plan_patch(&ws, &ledger, patch).err().map(|e| e.code()),
        Some(ErrorCode::StaleRead)
    );
    // An add over a file that exists and was not read is refused.
    let patch = "*** Begin Patch\n*** Add File: unread.txt\n+new\n*** End Patch";
    assert_eq!(
        plan_patch(&ws, &ledger, patch).err().map(|e| e.code()),
        Some(ErrorCode::StaleRead)
    );
    assert_eq!(content(&dir, "unread.txt"), "u\n");
}

#[test]
fn envelope_cannot_touch_git_or_denied_paths() {
    let (dir, ws, ledger) = setup(&["secrets"]);
    seed(&dir, ".git/config", "[core]\n");
    seed(&dir, "secrets/key.txt", "k\n");
    seed(&dir, "ok.txt", "ok\n");
    read_all(&ws, &ledger, &["ok.txt"]);
    for (patch, code) in [
        (
            "*** Begin Patch\n*** Update File: .git/config\n@@\n-[core]\n+[evil]\n*** End Patch",
            ErrorCode::Denied,
        ),
        (
            "*** Begin Patch\n*** Add File: .git/hooks/pre-commit\n+#!/bin/sh\n*** End Patch",
            ErrorCode::Denied,
        ),
        (
            "*** Begin Patch\n*** Update File: ok.txt\n@@\n-ok\n+fine\n*** Delete File: secrets/key.txt\n*** End Patch",
            ErrorCode::Denied,
        ),
        (
            "*** Begin Patch\n*** Update File: ok.txt\n*** Move to: secrets/ok.txt\n@@\n-ok\n+fine\n*** End Patch",
            ErrorCode::Denied,
        ),
        (
            "*** Begin Patch\n*** Update File: ok.txt\n@@\n-ok\n+fine\n*** Add File: secrets/new.txt\n+x\n*** End Patch",
            ErrorCode::Denied,
        ),
    ] {
        assert_eq!(
            plan_patch(&ws, &ledger, patch).err().map(|e| e.code()),
            Some(code),
            "{patch}"
        );
    }
    assert_eq!(content(&dir, ".git/config"), "[core]\n");
    assert_eq!(content(&dir, "secrets/key.txt"), "k\n");
    assert_eq!(content(&dir, "ok.txt"), "ok\n");
    assert!(!dir.path().join(".git/hooks/pre-commit").exists());
    assert!(!dir.path().join("secrets/new.txt").exists());
}

#[test]
fn envelope_rejects_absolute_and_parent_paths() {
    let (dir, ws, ledger) = setup(&[]);
    seed(&dir, "ok.txt", "ok\n");
    read_all(&ws, &ledger, &["ok.txt"]);
    let inside = dir.path().join("ok.txt");
    let absolute = format!(
        "*** Begin Patch\n*** Update File: {}\n@@\n-ok\n+fine\n*** End Patch",
        inside.display()
    );
    for patch in [
        absolute.as_str(),
        "*** Begin Patch\n*** Add File: /tmp/elitea-escape.txt\n+x\n*** End Patch",
        "*** Begin Patch\n*** Add File: ../escape.txt\n+x\n*** End Patch",
        "*** Begin Patch\n*** Update File: ok.txt\n*** Move to: ../moved.txt\n@@\n-ok\n+fine\n*** End Patch",
        "*** Begin Patch\n*** Delete File: sub/../../ok.txt\n*** End Patch",
        // `..` that would resolve back inside the workspace is refused too.
        "*** Begin Patch\n*** Add File: src/../secrets/x\n+x\n*** End Patch",
        "*** Begin Patch\n*** Update File: sub/../ok.txt\n@@\n-ok\n+fine\n*** End Patch",
        "*** Begin Patch\n*** Update File: ok.txt\n*** Move to: a\\..\\b.txt\n@@\n-ok\n+fine\n*** End Patch",
        "*** Begin Patch\n*** Add File: ~/escape.txt\n+x\n*** End Patch",
    ] {
        assert_eq!(
            plan_patch(&ws, &ledger, patch).err().map(|e| e.code()),
            Some(ErrorCode::OutsideWorkspace),
            "{patch}"
        );
    }
    assert_eq!(content(&dir, "ok.txt"), "ok\n");
    assert!(
        !dir.path()
            .parent()
            .expect("parent")
            .join("escape.txt")
            .exists()
    );
}

#[test]
fn envelope_ambiguous_fuzzy_fails_with_candidates() {
    let (dir, ws, ledger) = setup(&[]);
    seed(&dir, "f.txt", "x = 1  \ny\nx = 1\t\ny\n");
    read_all(&ws, &ledger, &["f.txt"]);
    let patch = "*** Begin Patch\n*** Update File: f.txt\n@@\n-x = 1\n+x = 2\n*** End Patch";
    let error = plan_patch(&ws, &ledger, patch).err().expect("ambiguous");
    assert_eq!(error.code(), ErrorCode::Conflict);
    assert!(
        error.message().contains("lines 1, 3"),
        "{}",
        error.message()
    );
}
