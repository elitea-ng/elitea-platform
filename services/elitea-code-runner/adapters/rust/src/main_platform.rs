//! Fixed broker-enabled wrapper. It requires a separate measured runtime profile.
pub mod platform;
mod user;
use std::io::{Read, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;

const MAX_RESULT_BYTES: usize = 256 * 1024;
struct Bounded(Vec<u8>);
impl Write for Bounded {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        if self
            .0
            .len()
            .checked_add(bytes.len())
            .is_none_or(|size| size > MAX_RESULT_BYTES)
        {
            return Err(std::io::Error::other("Code result exceeds 256 KiB"));
        }
        self.0.extend_from_slice(bytes);
        Ok(bytes.len())
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

#[derive(serde::Serialize)]
struct ResultEnvelope<'a> {
    result: &'a serde_json::Value,
    revision: u8,
}

fn write_result(path: &Path, result: &serde_json::Value) -> Result<(), Box<dyn std::error::Error>> {
    // Borrow the result: serialization cannot clone it before applying the byte cap.
    let mut output = Bounded(Vec::new());
    serde_json::to_writer(
        &mut output,
        &ResultEnvelope {
            result,
            revision: 1,
        },
    )?;
    // Exclusive creation atomically refuses existing files and symlinks, including dangling links.
    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(path)?;
    if !file.metadata()?.is_file() {
        return Err("Code result is not a regular file".into());
    }
    file.write_all(&output.0)?;
    Ok(())
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let mut bytes = Vec::new();
    std::fs::File::open("/workspace/.elitea-code.json")?
        .take(1024 * 1024 + 1)
        .read_to_end(&mut bytes)?;
    if bytes.len() > 1024 * 1024 {
        return Err("Code request exceeds its limit".into());
    }
    let prepared: serde_json::Value = serde_json::from_slice(&bytes)?;
    if prepared["revision"] != 5 || prepared["platform_client"]["revision"] != 1 {
        return Err("Code broker request identity is invalid".into());
    }
    let maximum = prepared["platform_client"]["max_calls"]
        .as_u64()
        .ok_or("Code broker policy is invalid")?;
    platform::initialize(maximum)?;
    let result = user::run(prepared["input"].clone())?;
    write_result(Path::new("/workspace/.elitea-code-result"), &result)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::{PermissionsExt, symlink};
    use std::path::PathBuf;
    use std::sync::atomic::{AtomicU64, Ordering};

    static NEXT_DIRECTORY: AtomicU64 = AtomicU64::new(0);
    struct Directory(PathBuf);
    impl Directory {
        fn new() -> Self {
            let path = std::env::temp_dir().join(format!(
                "elitea-platform-result-test-{}-{}",
                std::process::id(),
                NEXT_DIRECTORY.fetch_add(1, Ordering::Relaxed),
            ));
            std::fs::create_dir(&path).unwrap();
            Self(path)
        }
        fn result(&self) -> PathBuf {
            self.0.join(".elitea-code-result")
        }
    }
    impl Drop for Directory {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.0);
        }
    }

    #[test]
    fn result_is_exclusive_regular_private_and_keeps_the_envelope() {
        let directory = Directory::new();
        let path = directory.result();
        let result = serde_json::json!({"count":9,"label":"beta","typed":true});
        write_result(&path, &result).unwrap();
        let metadata = std::fs::symlink_metadata(&path).unwrap();
        assert!(metadata.is_file());
        assert_eq!(metadata.permissions().mode() & 0o777, 0o600);
        assert_eq!(
            std::fs::read(&path).unwrap(),
            serde_json::to_vec(&serde_json::json!({"revision":1,"result":result})).unwrap(),
        );
    }

    #[test]
    fn existing_regular_result_is_not_truncated_or_replaced() {
        let directory = Directory::new();
        let path = directory.result();
        std::fs::write(&path, b"previous result").unwrap();
        assert!(write_result(&path, &serde_json::Value::Null).is_err());
        assert_eq!(std::fs::read(path).unwrap(), b"previous result");
    }

    #[test]
    fn existing_and_dangling_symlinks_never_reach_the_target() {
        for existing in [true, false] {
            let directory = Directory::new();
            let target = directory.0.join("target");
            if existing {
                std::fs::write(&target, b"untouched target").unwrap();
            }
            let path = directory.result();
            symlink(&target, &path).unwrap();
            assert!(write_result(&path, &serde_json::json!({"new":true})).is_err());
            assert!(
                std::fs::symlink_metadata(path)
                    .unwrap()
                    .file_type()
                    .is_symlink()
            );
            if existing {
                assert_eq!(std::fs::read(target).unwrap(), b"untouched target");
            } else {
                assert!(!target.exists());
            }
        }
    }

    #[test]
    fn directory_result_is_refused_without_replacing_it() {
        let directory = Directory::new();
        let path = directory.result();
        std::fs::create_dir(&path).unwrap();
        assert!(write_result(&path, &serde_json::Value::Null).is_err());
        assert!(path.is_dir());
    }

    #[test]
    fn oversized_result_is_refused_before_creating_any_output() {
        let directory = Directory::new();
        let path = directory.result();
        let result = serde_json::Value::String("x".repeat(MAX_RESULT_BYTES + 1));
        assert!(write_result(&path, &result).is_err());
        assert!(std::fs::symlink_metadata(path).is_err());
        assert_eq!(std::fs::read_dir(&directory.0).unwrap().count(), 0);
    }

    #[test]
    fn envelope_overhead_is_included_in_the_exact_byte_limit() {
        let directory = Directory::new();
        let empty = serde_json::Value::String(String::new());
        let overhead = serde_json::to_vec(&ResultEnvelope {
            result: &empty,
            revision: 1,
        })
        .unwrap()
        .len();
        let result = serde_json::Value::String("x".repeat(MAX_RESULT_BYTES - overhead));
        let path = directory.result();
        write_result(&path, &result).unwrap();
        assert_eq!(
            std::fs::metadata(&path).unwrap().len(),
            MAX_RESULT_BYTES as u64
        );
        let overflow = serde_json::Value::String("x".repeat(MAX_RESULT_BYTES - overhead + 1));
        let overflow_path = directory.0.join("overflow");
        assert!(write_result(&overflow_path, &overflow).is_err());
        assert!(!overflow_path.exists());
    }
}
