//! Where a parse reads its files' bytes from: [`Sources`].
//!
//! [`crate::LanguageParser::parse_files`] reads each path from disk, as
//! the engines always have. [`crate::LanguageParser::parse_sources`] takes
//! the bytes a caller already read — the desktop's local index reads every
//! file through its confined workspace (`O_NOFOLLOW`, `path_deny`) and must
//! not have a parser open the same path again, which would follow a symlink
//! swapped in after the listing. Whatever the source, the bytes are decoded
//! and parsed exactly the same way, so the two give the same result for the
//! same bytes.
//!
//! The paths stay the keys the results and every cross-file registry use;
//! only where their bytes come from changes. (The JavaScript import
//! resolution still checks which modules exist with `lstat`, which reads no
//! content and follows no link.)

use std::borrow::Cow;
use std::collections::BTreeMap;
use std::io;

/// A caller's stop request, asked before each file is read.
pub type Stop<'a> = &'a (dyn Fn() -> bool + Sync);

/// The error text of a file not read because the parse was stopped.
pub const STOPPED: &str = "the parse was stopped";

/// The bytes of the files a parse reads.
#[derive(Clone, Copy)]
pub enum Sources<'a> {
    /// Each path is read from disk.
    Disk,
    /// Each path's bytes, as the caller read them. A path absent here is
    /// "not found": it is never read from disk instead.
    Memory(&'a BTreeMap<String, &'a [u8]>),
    /// [`Self::Memory`], and once `stop` answers `true` no further file is
    /// read: each fails at once with [`STOPPED`], so a stopped parse ends
    /// after the files already being parsed instead of parsing them all.
    /// The caller discards the results then.
    MemoryUntil(&'a BTreeMap<String, &'a [u8]>, Stop<'a>),
}

impl std::fmt::Debug for Sources<'_> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Disk => f.write_str("Disk"),
            Self::Memory(files) | Self::MemoryUntil(files, _) => {
                f.debug_tuple("Memory").field(&files.len()).finish()
            }
        }
    }
}

impl<'a> Sources<'a> {
    /// The bytes of `path`.
    pub(crate) fn read(self, path: &str) -> io::Result<Cow<'a, [u8]>> {
        let memory = |files: &'a BTreeMap<String, &'a [u8]>| {
            files
                .get(path)
                .map(|bytes| Cow::Borrowed(*bytes))
                .ok_or_else(|| io::Error::from(io::ErrorKind::NotFound))
        };
        match self {
            Self::Disk => std::fs::read(path).map(Cow::Owned),
            Self::MemoryUntil(_, stop) if stop() => {
                Err(io::Error::new(io::ErrorKind::Interrupted, STOPPED))
            }
            Self::Memory(files) | Self::MemoryUntil(files, _) => memory(files),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn memory_never_falls_back_to_the_disk() {
        let dir = std::env::temp_dir().join(format!("cp-input-{}", std::process::id()));
        let _ = std::fs::create_dir_all(&dir);
        let on_disk = dir.join("a.py");
        assert!(std::fs::write(&on_disk, "x = 1\n").is_ok());
        let path = on_disk.to_string_lossy().into_owned();
        let files = BTreeMap::from([("b.py".to_owned(), b"y = 2\n".as_slice())]);
        let memory = Sources::Memory(&files);
        assert_eq!(
            memory.read("b.py").ok().as_deref(),
            Some(b"y = 2\n".as_slice())
        );
        assert_eq!(
            memory.read(&path).map_err(|e| e.kind()).err(),
            Some(io::ErrorKind::NotFound),
            "a file on disk but not passed in is not read"
        );
        assert_eq!(
            Sources::Disk.read(&path).ok().as_deref(),
            Some(b"x = 1\n".as_slice())
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Every parser gives the same result for the same bytes, read from
    /// disk or passed in; and passed-in bytes are what is parsed, whatever
    /// the disk holds at that path.
    #[test]
    fn every_parser_parses_the_bytes_passed_in_as_it_parses_the_disk() {
        let cases = [
            (
                "python",
                "a.py",
                "class A:\n    def m(self):\n        return B()\n",
            ),
            (
                "javascript",
                "a.js",
                "export class A { m() { return b(); } }\n",
            ),
            (
                "typescript",
                "a.ts",
                "export class A { m(): number { return b(); } }\n",
            ),
            ("java", "A.java", "class A { void m() { b(); } }\n"),
            ("kotlin", "a.kt", "class A { fun m() { b() } }\n"),
            ("csharp", "a.cs", "class A { void M() { B(); } }\n"),
            ("go", "a.go", "package a\nfunc M() { b() }\n"),
            (
                "rust",
                "a.rs",
                "pub struct A;\nimpl A { pub fn m(&self) { b(); } }\n",
            ),
            ("swift", "a.swift", "class A { func m() { b() } }\n"),
            ("cpp", "a.cpp", "class A { void m() { b(); } };\n"),
        ];
        let dir = std::env::temp_dir().join(format!("cp-input-all-{}", std::process::id()));
        let _ = std::fs::create_dir_all(&dir);
        for (language, name, text) in cases {
            let path = dir.join(name).to_string_lossy().into_owned();
            assert!(std::fs::write(&path, text).is_ok());
            let Some(parser) = crate::parser_for(language) else {
                unreachable!("no parser for {language}")
            };
            let files = [path.clone()];
            let from_disk = parser.parse_files(&files);
            let bytes = BTreeMap::from([(path.clone(), text.as_bytes())]);
            let passed_in = parser.parse_sources(&files, Sources::Memory(&bytes));
            assert_eq!(from_disk, passed_in, "{language}");
            assert!(
                from_disk[&path].errors.is_empty() && !from_disk[&path].symbols.is_empty(),
                "{language}: {:?}",
                from_disk[&path]
            );
            // The disk now says something else: the bytes passed in win.
            assert!(std::fs::write(&path, "").is_ok());
            let still = parser.parse_sources(&files, Sources::Memory(&bytes));
            assert_eq!(still, passed_in, "{language}: the disk was read");
        }
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// A stopped parse reads no further file: every file read after the
    /// stop fails at once, whatever its language.
    #[test]
    fn a_stopped_parse_reads_no_further_file() {
        use std::sync::atomic::{AtomicUsize, Ordering};
        let files: Vec<String> = (0..40).map(|n| format!("/m/f{n}.py")).collect();
        let text = b"class A:\n    def m(self):\n        return 1\n".as_slice();
        let bytes: BTreeMap<String, &[u8]> = files.iter().map(|f| (f.clone(), text)).collect();
        let asked = AtomicUsize::new(0);
        // Stop after the fifth file asked to be read.
        let stop = || asked.fetch_add(1, Ordering::SeqCst) >= 5;
        let Some(parser) = crate::parser_for("python") else {
            unreachable!("python")
        };
        let results = parser.parse_sources(&files, Sources::MemoryUntil(&bytes, &stop));
        let stopped = results
            .values()
            .filter(|r| r.errors.iter().any(|e| e.contains(STOPPED)))
            .count();
        assert_eq!(stopped, 35, "{results:?}");
        let never = || false;
        let all = parser.parse_sources(&files, Sources::MemoryUntil(&bytes, &never));
        assert_eq!(all, parser.parse_sources(&files, Sources::Memory(&bytes)));
    }
}
