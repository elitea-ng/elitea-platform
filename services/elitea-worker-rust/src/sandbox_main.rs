//! Dedicated supervisor entry point. Build only with sandbox-supervisor.
use elitea_worker_rust::{diagnostics, sandbox::process};
use std::{ffi::OsString, path::PathBuf, process::ExitCode};

fn profile_paths(args: &[OsString]) -> Option<Vec<PathBuf>> {
    if args.is_empty()
        || args.len() > process::MAX_RUNTIME_PROFILES * 2
        || !args
            .chunks(2)
            .all(|pair| pair.len() == 2 && pair[0] == "--config")
    {
        return None;
    }
    Some(
        args.chunks_exact(2)
            .map(|pair| PathBuf::from(&pair[1]))
            .collect(),
    )
}

#[tokio::main(flavor = "multi_thread", worker_threads = 2)]
async fn main() -> ExitCode {
    diagnostics::install_redacted_panic_hook();
    if let Err(error) = diagnostics::install_tls_crypto_provider() {
        eprintln!("{error}");
        return ExitCode::FAILURE;
    }
    let mut tracing = match diagnostics::install_tracing_subscriber() {
        Ok(value) => value,
        Err(error) => {
            eprintln!("{error}");
            return ExitCode::FAILURE;
        }
    };
    let args: Vec<_> = std::env::args_os().skip(1).collect();
    let exit = if args.len() == 3 && args[0] == "--prepare-material" {
        match elitea_worker_rust::sandbox::material::prepare(
            &PathBuf::from(&args[1]),
            &PathBuf::from(&args[2]),
        ) {
            Ok(()) => ExitCode::SUCCESS,
            Err(error) => {
                tracing::error!(event="sandbox_material_failed", reason=%error);
                ExitCode::FAILURE
            }
        }
    } else if let Some(paths) = profile_paths(&args) {
        match process::run_profiles(&paths).await {
            Ok(()) => ExitCode::SUCCESS,
            Err(error) => {
                tracing::error!(event="sandbox_supervisor_exit", reason=%error);
                ExitCode::FAILURE
            }
        }
    } else {
        eprintln!(
            "usage: elitea-sandbox-supervisor --config <absolute-path> [--config <absolute-path> ...]"
        );
        ExitCode::FAILURE
    };
    if tracing.shutdown().is_err() {
        eprintln!("sandbox telemetry shutdown failed");
    }
    exit
}

#[cfg(test)]
mod tests {
    use super::profile_paths;
    use std::ffi::OsString;

    fn arguments(count: usize) -> Vec<OsString> {
        (0..count)
            .flat_map(|index| {
                [
                    OsString::from("--config"),
                    OsString::from(format!("/run/elitea/profile-{index}.json")),
                ]
            })
            .collect()
    }

    #[test]
    fn accepts_seven_and_eight_runtime_profiles() {
        for count in [7, 8] {
            assert_eq!(
                profile_paths(&arguments(count)).map(|p| p.len()),
                Some(count)
            );
        }
    }

    #[test]
    fn rejects_ninth_profile_and_malformed_arguments() {
        assert!(profile_paths(&arguments(9)).is_none());
        assert!(profile_paths(&[]).is_none());
        assert!(profile_paths(&[OsString::from("--config")]).is_none());
        assert!(profile_paths(&[OsString::from("--other"), OsString::from("/a")]).is_none());
    }
}
