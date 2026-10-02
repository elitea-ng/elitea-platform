//! Inert Kubernetes preparation and dispatch. No user code runs in these commands.
use serde::Deserialize;
use std::{
    fs::{self, OpenOptions},
    io::{self, Read, Write},
    path::Path,
};

const INPUT_LIMIT: u64 = 8 * 1024 * 1024;
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Input {
    pod_uid: String,
    request_digest: String,
    code: Option<String>,
    job: Option<String>,
}

fn invalid() -> io::Error {
    io::Error::other("sandbox preparation or runtime identity is invalid")
}

fn validate(input: &Input, uid: &str, digest: &str) -> io::Result<()> {
    if uid.is_empty()
        || input.pod_uid != uid
        || input.request_digest != digest
        || digest.len() != 64
        || !digest
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
    {
        return Err(invalid());
    }
    Ok(())
}

fn atomic_file(root: &Path, name: &str, bytes: &[u8]) -> io::Result<()> {
    let temporary = root.join(format!("{name}.{}", std::process::id()));
    let result = (|| {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temporary)?;
        file.write_all(bytes)?;
        file.sync_all()?;
        fs::rename(&temporary, root.join(name))
    })();
    // Never remove another helper's temporary file after a create conflict.
    if result.is_err()
        && result
            .as_ref()
            .err()
            .is_none_or(|e| e.kind() != io::ErrorKind::AlreadyExists)
    {
        let _ = fs::remove_file(&temporary);
    }
    result
}

fn ready(root: &Path, digest: &str) -> io::Result<bool> {
    match fs::File::open(root.join(".elitea-ready")) {
        Ok(file) => {
            let mut value = String::new();
            file.take(65).read_to_string(&mut value)?;
            if value != digest {
                return Err(invalid());
            }
            Ok(true)
        }
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(false),
        Err(error) => Err(error),
    }
}

fn apply(command: &str, input: Input, root: &Path, uid: &str, digest: &str) -> io::Result<()> {
    validate(&input, uid, digest)?;
    match command {
        "--prepare" => {
            let code = input.code.as_deref().ok_or_else(invalid)?;
            let job = input.job.as_deref().ok_or_else(invalid)?;
            if code.is_empty()
                || code.len() > 1024 * 1024
                || job.is_empty()
                || job.len() > 64 * 1024
            {
                return Err(invalid());
            }
            if ready(root, digest)? {
                return Ok(());
            }
            if root.join(".elitea-dispatch").exists() {
                return Err(invalid());
            }
            atomic_file(root, ".elitea-code.json", code.as_bytes())?;
            atomic_file(root, ".elitea-job.json", job.as_bytes())?;
            atomic_file(root, ".elitea-ready", digest.as_bytes())
        }
        "--prepared" | "--dispatch" => {
            if input.code.is_some() || input.job.is_some() || !ready(root, digest)? {
                return Err(invalid());
            }
            if command == "--dispatch" {
                match OpenOptions::new()
                    .write(true)
                    .create_new(true)
                    .open(root.join(".elitea-dispatch"))
                {
                    Ok(file) => file.sync_all()?,
                    Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {}
                    Err(error) => return Err(error),
                }
            }
            Ok(())
        }
        _ => Err(invalid()),
    }
}

pub fn run(command: &str) -> io::Result<()> {
    let mut bytes = Vec::new();
    io::stdin().take(INPUT_LIMIT + 1).read_to_end(&mut bytes)?;
    if bytes.len() > INPUT_LIMIT as usize {
        return Err(invalid());
    }
    let input = serde_json::from_slice(&bytes).map_err(|_| invalid())?;
    let uid = std::env::var("ELITEA_SANDBOX_POD_UID").map_err(|_| invalid())?;
    let digest = std::env::var("ELITEA_SANDBOX_REQUEST").map_err(|_| invalid())?;
    apply(command, input, Path::new("/workspace"), &uid, &digest)
}

#[cfg(test)]
mod tests {
    use super::*;
    static COUNTER: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
    struct Directory(std::path::PathBuf);
    impl Directory {
        fn new() -> Self {
            let path = std::env::temp_dir().join(format!(
                "elitea-lifecycle-{}-{}",
                std::process::id(),
                COUNTER.fetch_add(1, std::sync::atomic::Ordering::SeqCst)
            ));
            fs::create_dir(&path).unwrap();
            Self(path)
        }
    }
    impl Drop for Directory {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }
    fn input(uid: &str, prepare: bool) -> Input {
        Input {
            pod_uid: uid.into(),
            request_digest: "a".repeat(64),
            code: prepare.then(|| "private source".into()),
            job: prepare.then(|| "runner request".into()),
        }
    }
    #[test]
    fn prepare_is_inert_and_dispatch_is_repeatable_without_rewriting_code() {
        let root = Directory::new();
        let digest = "a".repeat(64);
        apply(
            "--prepare",
            input("original", true),
            &root.0,
            "original",
            &digest,
        )
        .unwrap();
        assert!(!root.0.join(".elitea-dispatch").exists());
        apply(
            "--dispatch",
            input("original", false),
            &root.0,
            "original",
            &digest,
        )
        .unwrap();
        apply(
            "--dispatch",
            input("original", false),
            &root.0,
            "original",
            &digest,
        )
        .unwrap();
        let mut retry = input("original", true);
        retry.code = Some("different".into());
        apply("--prepare", retry, &root.0, "original", &digest).unwrap();
        assert_eq!(
            fs::read_to_string(root.0.join(".elitea-code.json")).unwrap(),
            "private source"
        );
    }
    #[test]
    fn replacement_pod_and_unprepared_dispatch_are_rejected() {
        let root = Directory::new();
        let digest = "a".repeat(64);
        assert!(
            apply(
                "--prepare",
                input("original", true),
                &root.0,
                "replacement",
                &digest
            )
            .is_err()
        );
        assert!(
            apply(
                "--dispatch",
                input("original", false),
                &root.0,
                "original",
                &digest
            )
            .is_err()
        );
        assert_eq!(fs::read_dir(&root.0).unwrap().count(), 0);
    }
    #[test]
    fn invalid_ready_marker_and_oversized_input_cannot_signal_execution() {
        let root = Directory::new();
        let digest = "a".repeat(64);
        fs::write(root.0.join(".elitea-ready"), "b".repeat(64)).unwrap();
        assert!(
            apply(
                "--dispatch",
                input("original", false),
                &root.0,
                "original",
                &digest
            )
            .is_err()
        );
        fs::remove_file(root.0.join(".elitea-ready")).unwrap();
        let mut large = input("original", true);
        large.code = Some("x".repeat(1024 * 1024 + 1));
        assert!(apply("--prepare", large, &root.0, "original", &digest).is_err());
        assert!(!root.0.join(".elitea-dispatch").exists());
    }
}
