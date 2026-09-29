//! Compile and execute only inside the admitted Rust runtime container.
#![forbid(unsafe_code)]
use std::{
    io::Read,
    path::Path,
    process::{Command, Stdio},
};
const ROOT: &str = "/workspace/rust-job";

fn run(mut command: Command) -> Result<(), Box<dyn std::error::Error>> {
    let mut child = command
        .stdout(Stdio::piped())
        .stderr(Stdio::inherit())
        .stdin(Stdio::null())
        .spawn()?;
    // Stream compiler/program diagnostics to the parent runner, without buffering.
    std::io::copy(
        &mut child.stdout.take().ok_or("Code output pipe unavailable")?,
        &mut std::io::stderr(),
    )?;
    if !child.wait()?.success() {
        return Err("Rust compilation or execution failed; see diagnostics".into());
    }
    Ok(())
}

fn execute() -> Result<(), Box<dyn std::error::Error>> {
    let mut bytes = Vec::new();
    std::fs::File::open("/workspace/.elitea-code.json")?
        .take(1024 * 1024 + 1)
        .read_to_end(&mut bytes)?;
    if bytes.len() > 1024 * 1024 {
        return Err("Code request exceeds its limit".into());
    }
    let request: serde_json::Value = serde_json::from_slice(&bytes)?;
    if request["revision"] != 1 || request["language"] != "rust" || !request["input"].is_object() {
        return Err("Invalid Rust Code request".into());
    }
    let source = request["source"].as_str().ok_or("Rust source is missing")?;
    if source.len() > 256 * 1024 {
        return Err("Rust source exceeds its limit".into());
    }
    std::fs::create_dir(ROOT)?;
    std::fs::create_dir(format!("{ROOT}/src"))?;
    std::fs::create_dir(format!("{ROOT}/tmp"))?;
    std::fs::create_dir(format!("{ROOT}/.cargo"))?;
    for file in [
        "Cargo.toml",
        "Cargo.lock",
        "src/main.rs",
        ".cargo/config.toml",
    ] {
        std::fs::copy(
            Path::new("/opt/elitea-rust").join(file),
            Path::new(ROOT).join(file),
        )?;
    }
    std::fs::write(format!("{ROOT}/src/user.rs"), source)?;
    std::fs::write(
        format!("{ROOT}/input.json"),
        serde_json::to_vec(&request["input"])?,
    )?;
    let mut compile = Command::new("/usr/local/cargo/bin/cargo");
    compile
        .current_dir(ROOT)
        .env_clear()
        .env("PATH", "/usr/local/cargo/bin:/usr/local/bin:/usr/bin:/bin")
        .env("RUSTUP_HOME", "/usr/local/rustup")
        .env("CARGO_HOME", "/workspace/cargo")
        .env("HOME", "/workspace")
        .env("TMPDIR", format!("{ROOT}/tmp"))
        .args(["build", "--locked", "--offline", "-j", "2"]);
    run(compile)?;
    let mut program = Command::new(format!("{ROOT}/target/debug/elitea-code-job"));
    program
        .current_dir(ROOT)
        .env_clear()
        .env("TMPDIR", format!("{ROOT}/tmp"));
    run(program)?;
    bytes.clear();
    std::fs::File::open(format!("{ROOT}/result.json"))?
        .take(256 * 1024 + 1)
        .read_to_end(&mut bytes)?;
    if bytes.len() > 256 * 1024 {
        return Err("Rust result exceeds 256 KiB".into());
    }
    let result: serde_json::Value = serde_json::from_slice(&bytes)?;
    println!("{}", serde_json::json!({"revision":1, "result":result}));
    Ok(())
}
fn main() {
    if let Err(error) = execute() {
        eprintln!("Rust Code failed: {error}");
        std::process::exit(2);
    }
}
