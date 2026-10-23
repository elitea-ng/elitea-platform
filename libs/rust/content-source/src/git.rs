//! A checked-out repository as a content source: every regular file is a
//! document keyed by its `/`-separated path, versioned by its content hash,
//! readable by the whole project. `.git` and symbolic links are not
//! documents (the provider APIs the Python engine read through listed
//! neither as a readable file).

use crate::{Acl, ContentSource, Document, DocumentRef, SourceError, content_version, mime_of};
use serde_json::Map;
use std::path::{Path, PathBuf};

/// A checked-out tree.
#[derive(Debug, Clone)]
pub struct GitSource {
    root: PathBuf,
}

impl GitSource {
    /// The tree at `root` (the clone's working directory).
    #[must_use]
    pub fn new(root: impl Into<PathBuf>) -> Self {
        Self { root: root.into() }
    }

    /// The tree's root.
    #[must_use]
    pub fn root(&self) -> &Path {
        &self.root
    }

    /// Every regular file's key, in byte order (git's tree order).
    ///
    /// # Errors
    ///
    /// The tree cannot be walked.
    pub fn keys(&self) -> Result<Vec<String>, SourceError> {
        let unavailable = |error: std::io::Error| {
            SourceError::Unavailable(format!("the checked-out tree cannot be listed: {error}"))
        };
        let mut found = Vec::new();
        let mut pending = vec![PathBuf::new()];
        while let Some(relative) = pending.pop() {
            for entry in std::fs::read_dir(self.root.join(&relative)).map_err(unavailable)? {
                let entry = entry.map_err(unavailable)?;
                let name = entry.file_name().to_string_lossy().into_owned();
                let kind = entry.file_type().map_err(unavailable)?;
                let path = relative.join(&name);
                if kind.is_dir() {
                    if !(relative.as_os_str().is_empty() && name == ".git") {
                        pending.push(path);
                    }
                } else if kind.is_file() {
                    found.push(
                        path.components()
                            .map(|part| part.as_os_str().to_string_lossy().into_owned())
                            .collect::<Vec<_>>()
                            .join("/"),
                    );
                }
            }
        }
        found.sort_unstable();
        Ok(found)
    }

    fn read(&self, key: &str) -> Result<Vec<u8>, SourceError> {
        if key.split('/').any(|part| part == ".." || part.is_empty()) {
            return Err(SourceError::NotFound(key.to_owned()));
        }
        std::fs::read(self.root.join(key)).map_err(|error| match error.kind() {
            std::io::ErrorKind::NotFound => SourceError::NotFound(key.to_owned()),
            _ => SourceError::Unavailable(format!("'{key}' cannot be read: {error}")),
        })
    }

    /// A document's reference, from its bytes.
    #[must_use]
    pub fn reference(key: &str, bytes: &[u8]) -> DocumentRef {
        DocumentRef {
            key: key.to_owned(),
            version: content_version(bytes),
            mime: mime_of(key).to_owned(),
            size: bytes.len() as u64,
            acl: Acl::Project,
        }
    }
}

/// The walk and the reads run on the blocking pool ([`crate::blocking`]).
impl ContentSource for GitSource {
    async fn list(&self) -> Result<Vec<DocumentRef>, SourceError> {
        let source = self.clone();
        crate::blocking(move || {
            let mut references = Vec::new();
            for key in source.keys()? {
                let bytes = source.read(&key)?;
                references.push(Self::reference(&key, &bytes));
            }
            Ok(references)
        })
        .await
    }

    async fn fetch(&self, key: &str) -> Result<Document, SourceError> {
        let (source, key) = (self.clone(), key.to_owned());
        crate::blocking(move || {
            let bytes = source.read(&key)?;
            Ok(Document {
                reference: Self::reference(&key, &bytes),
                title: key.rsplit('/').next().unwrap_or(&key).to_owned(),
                uri: None,
                bytes,
                metadata: Map::new(),
            })
        })
        .await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn a_tree_lists_and_fetches_its_files() {
        let root = std::env::temp_dir().join(format!("content-source-git-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&root);
        for (path, text) in [
            ("src/a.py", "x = 1\n"),
            ("README.md", "# hi\n"),
            (".git/config", "[core]\n"),
        ] {
            let full = root.join(path);
            std::fs::create_dir_all(full.parent().unwrap_or(&root)).ok();
            std::fs::write(full, text).ok();
        }
        let source = GitSource::new(&root);
        let listed = source.list().await.unwrap_or_default();
        let keys: Vec<&str> = listed.iter().map(|d| d.key.as_str()).collect();
        assert_eq!(keys, ["README.md", "src/a.py"]);
        assert_eq!(listed[0].mime, "text/markdown");
        assert_eq!(listed[0].acl, Acl::Project);
        assert_eq!(listed[1].version, content_version(b"x = 1\n"));
        let document = source.fetch("src/a.py").await.ok();
        assert_eq!(document.map(|d| d.bytes), Some(b"x = 1\n".to_vec()));
        assert!(matches!(
            source.fetch("../etc/passwd").await,
            Err(SourceError::NotFound(_))
        ));
        assert!(matches!(
            source.fetch("nope.py").await,
            Err(SourceError::NotFound(_))
        ));
        let _ = std::fs::remove_dir_all(&root);
    }

    /// A fetch that blocks on its read (a FIFO no one writes to yet) leaves
    /// a current-thread runtime free: a timer started beside it fires
    /// first. Inline, the read held the runtime's one thread and the timer
    /// fired only after it.
    #[cfg(unix)]
    #[test]
    fn a_slow_read_does_not_hold_the_runtime() {
        use std::time::{Duration, Instant};
        let root = std::env::temp_dir().join(format!("content-source-fifo-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&root);
        std::fs::create_dir_all(&root).unwrap();
        let fifo = root.join("slow.txt");
        let made = std::process::Command::new("mkfifo")
            .arg(&fifo)
            .status()
            .unwrap();
        assert!(made.success());
        // The writer comes late, whatever the runtime does.
        let writer = {
            let fifo = fifo.clone();
            std::thread::spawn(move || {
                std::thread::sleep(Duration::from_millis(300));
                std::fs::write(fifo, b"late\n").unwrap();
            })
        };
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_time()
            .build()
            .unwrap();
        let source = GitSource::new(&root);
        let (fetched, read_done, timer_done) = runtime.block_on(async {
            let read = async {
                let document = source.fetch("slow.txt").await;
                (document, Instant::now())
            };
            let timer = async {
                tokio::time::sleep(Duration::from_millis(10)).await;
                Instant::now()
            };
            let ((document, read_done), timer_done) = tokio::join!(read, timer);
            (document, read_done, timer_done)
        });
        writer.join().unwrap();
        let _ = std::fs::remove_dir_all(&root);
        assert_eq!(fetched.unwrap().bytes, b"late\n");
        assert!(
            timer_done < read_done,
            "the timer fired only after the read returned"
        );
    }
}
