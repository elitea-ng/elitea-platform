//! Trusted Cargo acquisition and inert native profile import.
#![forbid(unsafe_code)]
mod rust_native;
mod rust_profile;
use rust_profile::rust_prepare_archive::digest;
use rust_profile::*;
use serde::Serialize;
use std::{io::Write, path::Path, time::Duration};
use tokio::time::Instant;
const REQUEST_PATH: &str = "/workspace/.elitea-rust-prepare.json";
const DECLARATION_PATH: &str = "/workspace/rust-dependencies.toml";
const OUTPUT_PATH: &str = "/workspace/rust-prepared";
const REQUEST_LIMIT: u64 = 4096;
const DECLARATION_LIMIT: usize = 64 * 1024;
#[derive(Serialize)]
struct Receipt {
    revision: u8,
    status: &'static str,
    code: Option<ErrorCode>,
    diagnostic: &'static str,
    record_sha256: Option<String>,
    archive_sha256: Option<String>,
}
async fn run_cli() -> Result<PreparationRecord> {
    let arguments: Vec<_> = std::env::args_os().skip(1).take(2).collect();
    if arguments.len() == 1 && arguments[0] == "--retain" {
        return rust_native::retain().await;
    }
    if arguments.len() == 1 && arguments[0] == "--hydrate" {
        return rust_native::hydrate_fixed();
    }
    let verify = arguments.len() == 1 && arguments[0] == "--verify";
    if !arguments.is_empty() && !verify {
        return Err(PrepareError::new(
            ErrorCode::InvalidRequest,
            "This helper accepts only fixed container files",
        ));
    }
    let request: Request =
        serde_json::from_slice(&read_regular(Path::new(REQUEST_PATH), REQUEST_LIMIT)?).map_err(
            |_| PrepareError::new(ErrorCode::InvalidRequest, "Preparation request is invalid"),
        )?;
    request.validate()?;
    let declaration = read_regular(Path::new(DECLARATION_PATH), DECLARATION_LIMIT as u64)?;
    if verify {
        let deadline = Instant::now() + Duration::from_secs(request.timeout_seconds);
        let record: PreparationRecord = serde_json::from_slice(&read_regular(
            &Path::new(OUTPUT_PATH).join("record.json"),
            8 * 1024 * 1024,
        )?)
        .map_err(|_| {
            PrepareError::new(ErrorCode::InvalidContent, "Preparation record is invalid")
        })?;
        verify_preparation(&declaration, Path::new(OUTPUT_PATH), &record, deadline)?;
        return Ok(record);
    }
    prepare(
        &declaration,
        Path::new(OUTPUT_PATH),
        Duration::from_secs(request.timeout_seconds),
        &CargoImage::container(),
    )
    .await
}

#[tokio::main(flavor = "current_thread")]
async fn main() {
    let (receipt, success) = match run_cli().await {
        Ok(record) => (
            Receipt {
                revision: 1,
                status: if std::env::args_os().nth(1).is_some() {
                    "verified"
                } else {
                    "prepared"
                },
                code: None,
                diagnostic: "Cargo dependencies are prepared",
                record_sha256: serde_json::to_vec(&record).ok().map(|bytes| digest(&bytes)),
                archive_sha256: Some(record.content.sha256),
            },
            true,
        ),
        Err(error) => (
            Receipt {
                revision: 1,
                status: "failed",
                code: Some(error.code),
                diagnostic: error.diagnostic,
                record_sha256: None,
                archive_sha256: None,
            },
            false,
        ),
    };
    let encoded = serde_json::to_vec(&receipt);
    let written = encoded.map_err(std::io::Error::other).and_then(|bytes| {
        let mut stdout = std::io::stdout().lock();
        stdout.write_all(&bytes)?;
        stdout.write_all(b"\n")?;
        stdout.flush()
    });
    if written.is_err() || !success {
        std::process::exit(2);
    }
}
