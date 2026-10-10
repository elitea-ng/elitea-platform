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

/// The bytes of the files a parse reads.
#[derive(Clone, Copy, Debug)]
pub enum Sources<'a> {
    /// Each path is read from disk.
    Disk,
    /// Each path's bytes, as the caller read them. A path absent here is
    /// "not found": it is never read from disk instead.
    Memory(&'a BTreeMap<String, &'a [u8]>),
}

impl<'a> Sources<'a> {
    /// The bytes of `path`.
    pub(crate) fn read(self, path: &str) -> io::Result<Cow<'a, [u8]>> {
        match self {
            Self::Disk => std::fs::read(path).map(Cow::Owned),
            Self::Memory(files) => files
                .get(path)
                .map(|bytes| Cow::Borrowed(*bytes))
                .ok_or_else(|| io::Error::from(io::ErrorKind::NotFound)),
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
}
