//! The model cache: `<app data>/models/<weights sha256>/`, owner-only,
//! shared by every workspace.
//!
//! A model is *ready* when each of its files is in its directory with the
//! catalogue's size and SHA-256. Files arrive by download (feature
//! `download`: from the Hugging Face CDN or a mirror, resumable — a
//! `<name>.part` grows across attempts and is hashed once complete) or by
//! import from a folder the person picked; either way a file is renamed
//! into place only after its digest matched. [`ModelStore::verified_dir`]
//! is what every load goes through: it checks each file against a marker
//! written when the file was verified (size and modification time), and
//! hashes again any file the marker does not vouch for. Nothing loads a
//! file that was not verified.

use crate::EmbedError;
use crate::catalog::{ModelFile, ModelSpec};
use crate::fsx::{create_private_dir, create_private_file, inspect_private_file};
use sha2::{Digest, Sha256};
use std::fmt::Write as _;
use std::fs::{File, OpenOptions};
use std::io::{self, Read, Write};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::UNIX_EPOCH;

/// The marker of verified files in a model's directory.
const MARKER: &str = "verified";

/// How much of a model is on this computer.
#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize)]
#[serde(tag = "state", rename_all = "snake_case")]
pub enum ModelState {
    Missing,
    /// Some bytes are here (finished files and `.part`s): a download
    /// resumes from them.
    Partial {
        have: u64,
        total: u64,
    },
    /// Every file is here at its size (verified on load).
    Ready,
}

/// A download's progress, over all the model's files.
#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize)]
pub struct Progress {
    pub model: &'static str,
    pub file: &'static str,
    pub done: u64,
    pub total: u64,
}

/// The model cache.
#[derive(Debug, Clone)]
pub struct ModelStore {
    root: PathBuf,
}

fn io_error(what: &str, error: &io::Error) -> EmbedError {
    EmbedError::Load(format!("{what}: {error}"))
}

fn hex(digest: &[u8]) -> String {
    digest.iter().fold(String::new(), |mut out, byte| {
        let _ = write!(out, "{byte:02x}");
        out
    })
}

fn mtime_ns(meta: &std::fs::Metadata) -> u128 {
    meta.modified()
        .ok()
        .and_then(|time| time.duration_since(UNIX_EPOCH).ok())
        .map_or(0, |since| since.as_nanos())
}

/// A regular file's metadata (never through a symlink); `None` when absent.
fn regular(path: &Path) -> Result<Option<std::fs::Metadata>, EmbedError> {
    match std::fs::symlink_metadata(path) {
        Ok(meta) if meta.is_file() => Ok(Some(meta)),
        Ok(_) => Err(EmbedError::Load(format!(
            "{} is not a regular file",
            path.display()
        ))),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(None),
        Err(error) => Err(io_error("a model file could not be read", &error)),
    }
}

/// Feed `reader` into `hasher`, returning the bytes read; stops (with
/// `Paused`) when `cancel` is set.
fn hash_into(
    reader: &mut impl Read,
    hasher: &mut Sha256,
    cancel: Option<&AtomicBool>,
) -> Result<u64, EmbedError> {
    let mut buffer = vec![0_u8; 1 << 20];
    let mut total = 0_u64;
    loop {
        if cancel.is_some_and(|cancel| cancel.load(Ordering::Relaxed)) {
            return Err(EmbedError::Paused);
        }
        let n = reader
            .read(&mut buffer)
            .map_err(|error| io_error("a model file could not be read", &error))?;
        if n == 0 {
            return Ok(total);
        }
        hasher.update(&buffer[..n]);
        total += n as u64;
    }
}

/// The SHA-256 of a file (lowercase hex).
///
/// # Errors
///
/// It cannot be read.
pub fn sha256_file(path: &Path) -> Result<String, EmbedError> {
    let mut file =
        File::open(path).map_err(|error| io_error("a model file could not be read", &error))?;
    let mut hasher = Sha256::new();
    hash_into(&mut file, &mut hasher, None)?;
    Ok(hex(&hasher.finalize()))
}

impl ModelStore {
    /// The cache under `root` (`<app data>/models`); created on first write.
    #[must_use]
    pub fn new(root: impl Into<PathBuf>) -> Self {
        Self { root: root.into() }
    }

    #[must_use]
    pub fn root(&self) -> &Path {
        &self.root
    }

    /// A model's directory.
    #[must_use]
    pub fn dir(&self, spec: &ModelSpec) -> PathBuf {
        self.root.join(spec.cache_key())
    }

    fn ensure_dir(&self, spec: &ModelSpec) -> Result<PathBuf, EmbedError> {
        create_private_dir(&self.root)
            .map_err(|error| io_error("the model cache could not be created", &error))?;
        let dir = self.dir(spec);
        create_private_dir(&dir)
            .map_err(|error| io_error("the model cache could not be created", &error))?;
        Ok(dir)
    }

    /// How much of `spec` is here (sizes only; no hashing).
    #[must_use]
    pub fn state(&self, spec: &ModelSpec) -> ModelState {
        let dir = self.dir(spec);
        let mut have = 0;
        let mut complete = true;
        for file in spec.files {
            match regular(&dir.join(file.name)) {
                Ok(Some(meta)) if meta.len() == file.size => have += file.size,
                _ => {
                    complete = false;
                    if let Ok(Some(meta)) = regular(&dir.join(part_name(file))) {
                        have += meta.len().min(file.size);
                    }
                }
            }
        }
        if complete {
            ModelState::Ready
        } else if have > 0 {
            ModelState::Partial {
                have,
                total: spec.download_bytes(),
            }
        } else {
            ModelState::Missing
        }
    }

    /// The model's directory once every file in it is verified: a file the
    /// marker vouches for (same size and modification time as when it was
    /// hashed) is not hashed again; any other is, and a mismatch is
    /// refused (the file is left for the person to remove or replace).
    ///
    /// # Errors
    ///
    /// A file is missing, the wrong size, or does not match its digest.
    pub fn verified_dir(&self, spec: &ModelSpec) -> Result<PathBuf, EmbedError> {
        let dir = self.dir(spec);
        let marker = read_marker(&dir);
        let mut lines = Vec::new();
        let mut rewrite = false;
        for file in spec.files {
            let path = dir.join(file.name);
            let meta = regular(&path)?
                .ok_or_else(|| EmbedError::Load(format!("{} is not downloaded", file.name)))?;
            if meta.len() != file.size {
                return Err(EmbedError::Load(format!(
                    "{} is {} bytes, expected {}",
                    file.name,
                    meta.len(),
                    file.size
                )));
            }
            let line = marker_line(file, &meta);
            if !marker.contains(&line) {
                inspect_private_file(&path)
                    .map_err(|error| io_error("a model file is refused", &error))?;
                let digest = sha256_file(&path)?;
                if digest != file.sha256 {
                    return Err(EmbedError::Load(format!(
                        "{} does not match its pinned SHA-256",
                        file.name
                    )));
                }
                rewrite = true;
            }
            lines.push(line);
        }
        if rewrite {
            write_marker(&dir, &lines)?;
        }
        Ok(dir)
    }

    /// Copy the model's files from `from` (a folder holding them by name),
    /// each verified before it is put in place.
    ///
    /// # Errors
    ///
    /// A file is missing in `from`, or does not match its digest.
    pub fn import(&self, spec: &ModelSpec, from: &Path) -> Result<(), EmbedError> {
        let dir = self.ensure_dir(spec)?;
        for file in spec.files {
            let source = from.join(file.name);
            let mut reader = File::open(&source).map_err(|error| {
                EmbedError::Load(format!("{} could not be read: {error}", source.display()))
            })?;
            let part = dir.join(part_name(file));
            let _ = std::fs::remove_file(&part);
            create_private_file(&part)
                .map_err(|error| io_error("the model cache could not be written", &error))?;
            let mut out = OpenOptions::new()
                .write(true)
                .truncate(true)
                .open(&part)
                .map_err(|error| io_error("the model cache could not be written", &error))?;
            let mut hasher = Sha256::new();
            let mut buffer = vec![0_u8; 1 << 20];
            loop {
                let n = reader
                    .read(&mut buffer)
                    .map_err(|error| io_error("a model file could not be read", &error))?;
                if n == 0 {
                    break;
                }
                hasher.update(&buffer[..n]);
                out.write_all(&buffer[..n])
                    .map_err(|error| io_error("the model cache could not be written", &error))?;
            }
            out.sync_all()
                .map_err(|error| io_error("the model cache could not be written", &error))?;
            finish(&dir, file, &part, &hex(&hasher.finalize()))?;
        }
        self.verified_dir(spec).map(|_| ())
    }

    /// Delete the model's directory (nothing loaded keeps using it: a
    /// loaded model holds its weights in memory).
    ///
    /// # Errors
    ///
    /// It could not be removed.
    pub fn remove(&self, spec: &ModelSpec) -> Result<(), EmbedError> {
        match std::fs::remove_dir_all(self.dir(spec)) {
            Ok(()) => Ok(()),
            Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(()),
            Err(error) => Err(io_error("the model could not be removed", &error)),
        }
    }

    /// Bytes the cache holds (every model, partial downloads included).
    #[must_use]
    pub fn disk_bytes(&self) -> u64 {
        fn walk(dir: &Path) -> u64 {
            std::fs::read_dir(dir).map_or(0, |entries| {
                entries
                    .flatten()
                    .map(|entry| match entry.file_type() {
                        Ok(kind) if kind.is_dir() => walk(&entry.path()),
                        Ok(kind) if kind.is_file() => entry.metadata().map_or(0, |m| m.len()),
                        _ => 0,
                    })
                    .sum()
            })
        }
        walk(&self.root)
    }
}

fn part_name(file: &ModelFile) -> String {
    format!("{}.part", file.name)
}

fn marker_line(file: &ModelFile, meta: &std::fs::Metadata) -> String {
    format!(
        "{}\t{}\t{}\t{}",
        file.name,
        meta.len(),
        mtime_ns(meta),
        file.sha256
    )
}

fn read_marker(dir: &Path) -> Vec<String> {
    let path = dir.join(MARKER);
    if !matches!(regular(&path), Ok(Some(_))) {
        return Vec::new();
    }
    std::fs::read_to_string(path)
        .map(|text| text.lines().map(str::to_owned).collect())
        .unwrap_or_default()
}

fn write_marker(dir: &Path, lines: &[String]) -> Result<(), EmbedError> {
    let path = dir.join(MARKER);
    let temporary = dir.join(format!("{MARKER}.tmp"));
    let _ = std::fs::remove_file(&temporary);
    create_private_file(&temporary)
        .map_err(|error| io_error("the model cache could not be written", &error))?;
    std::fs::write(&temporary, lines.join("\n") + "\n")
        .and_then(|()| std::fs::rename(&temporary, &path))
        .map_err(|error| io_error("the model cache could not be written", &error))
}

/// Put a completed `.part` in place once its digest matched; a mismatch
/// deletes it (a resumed download that went wrong starts over).
fn finish(dir: &Path, file: &ModelFile, part: &Path, digest: &str) -> Result<(), EmbedError> {
    if digest != file.sha256 {
        let _ = std::fs::remove_file(part);
        return Err(EmbedError::Load(format!(
            "{} does not match its pinned SHA-256",
            file.name
        )));
    }
    let target = dir.join(file.name);
    std::fs::rename(part, &target)
        .map_err(|error| io_error("the model cache could not be written", &error))?;
    // The marker vouches for it from now on.
    let meta = regular(&target)?.ok_or_else(|| EmbedError::Load("a model file vanished".into()))?;
    let mut lines = read_marker(dir);
    lines.retain(|line| !line.starts_with(&format!("{}\t", file.name)));
    lines.push(marker_line(file, &meta));
    write_marker(dir, &lines)
}

#[cfg(feature = "download")]
mod download {
    use super::{
        EmbedError, ModelFile, ModelSpec, ModelStore, Progress, finish, hash_into, hex, part_name,
        regular,
    };
    use crate::fsx::create_private_file;
    use sha2::{Digest, Sha256};
    use std::fs::OpenOptions;
    use std::io::Write;
    use std::path::Path;
    use std::sync::atomic::{AtomicBool, Ordering};

    /// Redirects followed per file (the Hub answers with one to its CDN).
    const MAX_REDIRECTS: usize = 8;
    /// Progress is reported at most once per this many bytes.
    const REPORT_EVERY: u64 = 4 << 20;

    fn failed(error: impl std::fmt::Display) -> EmbedError {
        EmbedError::Remote(format!("the model download failed: {error}"))
    }

    impl ModelStore {
        /// Download the model's missing files from `base` (the Hub,
        /// `https://huggingface.co`, or a mirror with its layout:
        /// `<base>/<repo>/resolve/<revision>/<file>`), resuming any
        /// `.part`, each verified before it is put in place.
        ///
        /// `client` should not follow redirects itself: they are followed
        /// here, never from `https` to anything else.
        ///
        /// # Errors
        ///
        /// The transfer failed or was refused (the `.part` is kept for the
        /// next attempt), a file does not match its digest (its `.part` is
        /// deleted), or `cancel` was set (`Paused`; the `.part` is kept).
        pub async fn download(
            &self,
            spec: &'static ModelSpec,
            client: &reqwest::Client,
            base: &str,
            progress: &(dyn Fn(Progress) + Send + Sync),
            cancel: &AtomicBool,
        ) -> Result<(), EmbedError> {
            let dir = self.ensure_dir(spec)?;
            let total = spec.download_bytes();
            let mut done = 0;
            for file in spec.files {
                if regular(&dir.join(file.name))?.is_some_and(|meta| meta.len() == file.size) {
                    done += file.size;
                    continue;
                }
                let url = spec.file_url(base, file);
                fetch(client, &url, &dir, file, cancel, &|have| {
                    progress(Progress {
                        model: spec.id,
                        file: file.name,
                        done: done + have,
                        total,
                    });
                })
                .await?;
                done += file.size;
            }
            progress(Progress {
                model: spec.id,
                file: "",
                done: total,
                total,
            });
            self.verified_dir(spec).map(|_| ())
        }
    }

    async fn fetch(
        client: &reqwest::Client,
        url: &str,
        dir: &Path,
        file: &'static ModelFile,
        cancel: &AtomicBool,
        report: &(dyn Fn(u64) + Send + Sync),
    ) -> Result<(), EmbedError> {
        let part = dir.join(part_name(file));
        create_private_file(&part).map_err(failed)?;
        let mut have = regular(&part)?.map_or(0, |meta| meta.len());
        if have > file.size {
            have = 0;
        }
        let mut hasher = Sha256::new();
        if have > 0 {
            let mut existing = std::fs::File::open(&part).map_err(failed)?;
            let mut limited = std::io::Read::take(&mut existing, have);
            have = hash_into(&mut limited, &mut hasher, Some(cancel))?;
        }
        let mut response = None;
        let mut target = reqwest::Url::parse(url).map_err(failed)?;
        for _ in 0..=MAX_REDIRECTS {
            let mut request = client.get(target.clone());
            if have > 0 {
                request = request.header(reqwest::header::RANGE, format!("bytes={have}-"));
            }
            let answer = request.send().await.map_err(failed)?;
            if answer.status().is_redirection() {
                let location = answer
                    .headers()
                    .get(reqwest::header::LOCATION)
                    .and_then(|value| value.to_str().ok())
                    .ok_or_else(|| failed("a redirect without a location"))?;
                let next = target.join(location).map_err(failed)?;
                if target.scheme() == "https" && next.scheme() != "https" {
                    return Err(failed("a redirect away from https"));
                }
                target = next;
                continue;
            }
            response = Some(answer);
            break;
        }
        let mut response = response.ok_or_else(|| failed("too many redirects"))?;
        let status = response.status();
        let append = if status == reqwest::StatusCode::PARTIAL_CONTENT && have > 0 {
            true
        } else if status == reqwest::StatusCode::RANGE_NOT_SATISFIABLE && have == file.size {
            false
        } else if status.is_success() {
            // A server that ignores the range sends it all again.
            have = 0;
            hasher = Sha256::new();
            false
        } else {
            return Err(failed(format!(
                "{} answered {status}",
                target.host_str().unwrap_or("")
            )));
        };
        let mut out = OpenOptions::new()
            .write(true)
            .append(append)
            .truncate(!append && have == 0)
            .open(&part)
            .map_err(failed)?;
        if status != reqwest::StatusCode::RANGE_NOT_SATISFIABLE {
            let mut reported = have;
            while let Some(chunk) = response.chunk().await.map_err(failed)? {
                if cancel.load(Ordering::Relaxed) {
                    return Err(EmbedError::Paused);
                }
                if have + chunk.len() as u64 > file.size {
                    let _ = std::fs::remove_file(&part);
                    return Err(failed(format!("{} is longer than pinned", file.name)));
                }
                out.write_all(&chunk).map_err(failed)?;
                hasher.update(&chunk);
                have += chunk.len() as u64;
                if have - reported >= REPORT_EVERY {
                    reported = have;
                    report(have);
                }
            }
        }
        out.sync_all().map_err(failed)?;
        drop(out);
        if have != file.size {
            return Err(failed(format!(
                "{} stopped at {have} of {} bytes",
                file.name, file.size
            )));
        }
        report(have);
        finish(dir, file, &part, &hex(&hasher.finalize()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::catalog::Family;

    pub(crate) fn spec_for(files: Vec<(&'static str, Vec<u8>)>) -> &'static ModelSpec {
        let files: Vec<ModelFile> = files
            .into_iter()
            .map(|(name, bytes)| ModelFile {
                name,
                sha256: Box::leak(hex(&Sha256::digest(&bytes)).into_boxed_str()),
                size: bytes.len() as u64,
            })
            .collect();
        Box::leak(Box::new(ModelSpec {
            id: "test-model",
            display_name: "Test",
            family: Family::Bert,
            repo: "org/test",
            revision: "abc",
            files: Box::leak(files.into_boxed_slice()),
            native_dimension: 4,
            dimension: 4,
            max_tokens: 16,
            ram_mb: 1,
            summary: "",
            license: "",
        }))
    }

    #[test]
    fn an_import_is_verified_and_a_changed_file_is_refused_on_load() {
        let cache = tempfile::tempdir().unwrap();
        let from = tempfile::tempdir().unwrap();
        let spec = spec_for(vec![
            ("model.safetensors", vec![7; 3000]),
            ("tokenizer.json", b"{}".to_vec()),
        ]);
        let store = ModelStore::new(cache.path().join("models"));
        assert_eq!(store.state(spec), ModelState::Missing);
        std::fs::write(from.path().join("model.safetensors"), vec![7; 3000]).unwrap();
        // A wrong file is refused and nothing is put in place.
        std::fs::write(from.path().join("tokenizer.json"), b"[]").unwrap();
        assert!(store.import(spec, from.path()).is_err());
        assert!(!store.dir(spec).join("tokenizer.json").exists());
        std::fs::write(from.path().join("tokenizer.json"), b"{}").unwrap();
        store.import(spec, from.path()).unwrap();
        assert_eq!(store.state(spec), ModelState::Ready);
        let dir = store.verified_dir(spec).unwrap();
        assert_eq!(dir, store.dir(spec));
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let mode = |p: &Path| std::fs::metadata(p).unwrap().permissions().mode() & 0o777;
            assert_eq!(mode(&dir), 0o700);
            assert_eq!(mode(&dir.join("model.safetensors")), 0o600);
        }
        // Same size, other bytes, other mtime: hashed again and refused.
        std::thread::sleep(std::time::Duration::from_millis(20));
        std::fs::write(dir.join("model.safetensors"), vec![8; 3000]).unwrap();
        let refused = store.verified_dir(spec).unwrap_err();
        assert!(refused.to_string().contains("SHA-256"), "{refused}");
        store.remove(spec).unwrap();
        assert_eq!(store.state(spec), ModelState::Missing);
    }
}

#[cfg(all(test, feature = "download"))]
mod download_tests {
    use super::tests::spec_for;
    use super::*;
    use std::io::{BufRead, BufReader};
    use std::net::TcpListener;
    use std::sync::Arc;
    use std::sync::atomic::AtomicUsize;

    /// A file server: `/org/test/resolve/abc/<name>` redirects to
    /// `/cdn/<name>`, which honours `Range`; the first `cut` answers stop
    /// after half the body.
    struct Server {
        base: String,
        ranges: Arc<std::sync::Mutex<Vec<Option<String>>>>,
    }

    fn serve(files: Vec<(&'static str, Vec<u8>)>, cut: usize) -> Server {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let base = format!("http://{}", listener.local_addr().unwrap());
        let ranges = Arc::new(std::sync::Mutex::new(Vec::new()));
        let seen = Arc::clone(&ranges);
        let cuts = Arc::new(AtomicUsize::new(cut));
        std::thread::spawn(move || {
            for stream in listener.incoming() {
                let Ok(mut stream) = stream else { continue };
                let mut reader = BufReader::new(stream.try_clone().unwrap());
                let mut line = String::new();
                reader.read_line(&mut line).unwrap();
                let path = line.split_whitespace().nth(1).unwrap_or("").to_owned();
                let mut range = None;
                loop {
                    let mut header = String::new();
                    reader.read_line(&mut header).unwrap();
                    if header.trim().is_empty() {
                        break;
                    }
                    if let Some(value) = header.to_ascii_lowercase().strip_prefix("range: bytes=") {
                        range = Some(value.trim().trim_end_matches('-').to_owned());
                    }
                }
                if let Some(name) = path.strip_prefix("/org/test/resolve/abc/") {
                    let reply = format!(
                        "HTTP/1.1 302 Found\r\nLocation: /cdn/{name}\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
                    );
                    stream.write_all(reply.as_bytes()).unwrap();
                    continue;
                }
                seen.lock().unwrap().push(range.clone());
                let name = path.trim_start_matches("/cdn/");
                let Some((_, body)) = files.iter().find(|(n, _)| *n == name) else {
                    let _ = stream.write_all(
                        b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
                    );
                    continue;
                };
                let from: usize = range.as_deref().and_then(|r| r.parse().ok()).unwrap_or(0);
                let rest = &body[from..];
                let status = if from > 0 {
                    "206 Partial Content"
                } else {
                    "200 OK"
                };
                let head = format!(
                    "HTTP/1.1 {status}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    rest.len()
                );
                stream.write_all(head.as_bytes()).unwrap();
                if cuts.load(Ordering::SeqCst) > 0 {
                    cuts.fetch_sub(1, Ordering::SeqCst);
                    let _ = stream.write_all(&rest[..rest.len() / 2]);
                    continue; // dropped mid-body
                }
                let _ = stream.write_all(rest);
            }
        });
        Server { base, ranges }
    }

    fn client() -> reqwest::Client {
        reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .build()
            .unwrap()
    }

    #[tokio::test]
    async fn a_cut_download_resumes_from_its_part_and_is_verified() {
        let weights: Vec<u8> = (0..200_000_u32).map(|i| (i % 251) as u8).collect();
        let files = vec![
            ("model.safetensors", weights),
            ("tokenizer.json", b"{\"a\":1}".to_vec()),
        ];
        let spec = spec_for(files.clone());
        let server = serve(files, 1);
        let cache = tempfile::tempdir().unwrap();
        let store = ModelStore::new(cache.path().join("models"));
        let cancel = AtomicBool::new(false);
        let reports = std::sync::Mutex::new(Vec::new());
        let progress = |p: Progress| reports.lock().unwrap().push(p.done);
        let first = store
            .download(spec, &client(), &server.base, &progress, &cancel)
            .await;
        assert!(first.is_err(), "the cut answer fails the first attempt");
        assert!(matches!(store.state(spec), ModelState::Partial { have, .. } if have > 0));
        store
            .download(spec, &client(), &server.base, &progress, &cancel)
            .await
            .unwrap();
        assert_eq!(store.state(spec), ModelState::Ready);
        store.verified_dir(spec).unwrap();
        let ranges = server.ranges.lock().unwrap().clone();
        assert_eq!(ranges[0], None);
        assert_eq!(ranges[1].as_deref(), Some("100000"), "resumed: {ranges:?}");
        assert_eq!(
            *reports.lock().unwrap().last().unwrap(),
            spec.download_bytes()
        );
    }

    #[tokio::test]
    async fn a_file_that_does_not_match_is_deleted_not_kept() {
        let bytes_served = vec![("model.safetensors", vec![1_u8; 5000])];
        let spec = spec_for(vec![("model.safetensors", vec![2_u8; 5000])]);
        let server = serve(bytes_served, 0);
        let cache = tempfile::tempdir().unwrap();
        let store = ModelStore::new(cache.path().join("models"));
        let cancel = AtomicBool::new(false);
        let error = store
            .download(spec, &client(), &server.base, &|_| {}, &cancel)
            .await
            .unwrap_err();
        assert!(error.to_string().contains("SHA-256"), "{error}");
        assert_eq!(store.state(spec), ModelState::Missing);
    }
}
