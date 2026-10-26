//! Which language servers exist: a static table, the workspace's languages
//! by their marker files at its root, and each server found on the host's
//! `PATH` (never downloaded).

use std::fmt;
use std::path::{Path, PathBuf};
use std::time::Duration;

use serde::{Deserialize, Serialize};

use crate::workspace::{EntryKind, Workspace, WsPath};

/// A language with a server in the table.
#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Language {
    Rust,
    /// TypeScript and JavaScript: one server for both.
    Typescript,
    Python,
    Go,
}

impl Language {
    pub const ALL: [Self; 4] = [Self::Rust, Self::Typescript, Self::Python, Self::Go];

    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Rust => "rust",
            Self::Typescript => "typescript",
            Self::Python => "python",
            Self::Go => "go",
        }
    }

    /// The name people know it by, for messages.
    #[must_use]
    pub const fn label(self) -> &'static str {
        match self {
            Self::Rust => "Rust",
            Self::Typescript => "TypeScript and JavaScript",
            Self::Python => "Python",
            Self::Go => "Go",
        }
    }

    /// The server's program name, looked up on `PATH`, and its arguments.
    #[must_use]
    pub const fn server(self) -> (&'static str, &'static [&'static str]) {
        match self {
            Self::Rust => ("rust-analyzer", &[]),
            Self::Typescript => ("typescript-language-server", &["--stdio"]),
            Self::Python => ("pyright-langserver", &["--stdio"]),
            Self::Go => ("gopls", &[]),
        }
    }

    /// The files at the workspace root that say the language is there.
    #[must_use]
    pub const fn markers(self) -> &'static [&'static str] {
        match self {
            Self::Rust => &["Cargo.toml"],
            Self::Typescript => &["package.json", "tsconfig.json", "jsconfig.json"],
            Self::Python => &["pyproject.toml", "setup.py", "setup.cfg"],
            Self::Go => &["go.mod"],
        }
    }

    /// How long an edit waits for the server's diagnostics by default; the
    /// first wait after a start is [`Self::first_wait`].
    #[must_use]
    pub const fn default_wait(self) -> Duration {
        Duration::from_millis(1_500)
    }

    /// The first wait after a start, which includes the server loading the
    /// project (rust-analyzer's is the longest).
    #[must_use]
    pub const fn first_wait(self) -> Duration {
        match self {
            Self::Rust => Duration::from_millis(4_000),
            _ => Duration::from_millis(3_000),
        }
    }
}

impl fmt::Display for Language {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// The language of a file and the LSP `languageId` of its document, by its
/// extension.
#[must_use]
pub fn language_of(path: &WsPath) -> Option<(Language, &'static str)> {
    let name = path.file_name()?;
    let extension = name.rsplit_once('.')?.1.to_ascii_lowercase();
    Some(match extension.as_str() {
        "rs" => (Language::Rust, "rust"),
        "ts" | "mts" | "cts" => (Language::Typescript, "typescript"),
        "tsx" => (Language::Typescript, "typescriptreact"),
        "js" | "mjs" | "cjs" => (Language::Typescript, "javascript"),
        "jsx" => (Language::Typescript, "javascriptreact"),
        "py" | "pyi" => (Language::Python, "python"),
        "go" => (Language::Go, "go"),
        _ => return None,
    })
}

/// Whether the workspace root holds one of `language`'s markers (a regular
/// file; a symlink does not count, as nothing is followed).
#[must_use]
pub fn detected(workspace: &Workspace, language: Language) -> bool {
    language.markers().iter().any(|marker| {
        WsPath::from_relative(Path::new(marker))
            .is_ok_and(|path| matches!(workspace.stat(&path), Ok(Some(EntryKind::File))))
    })
}

/// The first executable `name` in `search` (absolute `PATH` entries), as
/// the absolute path found there. Not canonicalised for running: a
/// rustup proxy is a link to `rustup`, which picks the tool by the name it
/// is run as.
#[must_use]
pub fn find_on_path(name: &str, search: &[PathBuf]) -> Option<PathBuf> {
    use std::os::unix::fs::PermissionsExt as _;
    search
        .iter()
        .filter(|dir| dir.is_absolute())
        .map(|dir| dir.join(name))
        .find(|path| {
            path.metadata()
                .is_ok_and(|meta| meta.is_file() && meta.permissions().mode() & 0o111 != 0)
        })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn extensions_map_to_their_server() {
        let path = |text: &str| WsPath::from_relative(Path::new(text)).unwrap();
        assert_eq!(
            language_of(&path("src/lib.rs")),
            Some((Language::Rust, "rust"))
        );
        assert_eq!(
            language_of(&path("web/App.TSX")),
            Some((Language::Typescript, "typescriptreact"))
        );
        assert_eq!(
            language_of(&path("a/b.mjs")),
            Some((Language::Typescript, "javascript"))
        );
        assert_eq!(
            language_of(&path("x.py")),
            Some((Language::Python, "python"))
        );
        assert_eq!(language_of(&path("main.go")), Some((Language::Go, "go")));
        assert_eq!(language_of(&path("README.md")), None);
        assert_eq!(language_of(&path("Makefile")), None);
    }

    #[test]
    fn markers_at_the_root_detect_a_language() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("go.mod"), "module x\n").unwrap();
        std::fs::create_dir(dir.path().join("Cargo.toml")).unwrap();
        let workspace = Workspace::open(dir.path(), &[]).unwrap();
        assert!(detected(&workspace, Language::Go));
        assert!(
            !detected(&workspace, Language::Rust),
            "a directory is no marker"
        );
        assert!(!detected(&workspace, Language::Python));
    }

    #[test]
    fn a_server_is_found_on_path_but_not_in_relative_entries() {
        use std::os::unix::fs::PermissionsExt as _;
        let dir = tempfile::tempdir().unwrap();
        let program = dir.path().join("gopls");
        std::fs::write(&program, "#!/bin/sh\n").unwrap();
        std::fs::set_permissions(&program, std::fs::Permissions::from_mode(0o755)).unwrap();
        let found = find_on_path("gopls", &[dir.path().to_path_buf()]);
        assert_eq!(found.as_deref(), Some(program.as_path()));
        assert_eq!(find_on_path("gopls", &[PathBuf::from("relative")]), None);
        assert_eq!(
            find_on_path("rust-analyzer", &[dir.path().to_path_buf()]),
            None
        );
    }
}
