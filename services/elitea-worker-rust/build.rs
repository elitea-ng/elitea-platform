use std::env;
use std::error::Error;
use std::path::PathBuf;

fn main() -> Result<(), Box<dyn Error>> {
    let manifest_dir = PathBuf::from(
        env::var_os("CARGO_MANIFEST_DIR")
            .ok_or("CARGO_MANIFEST_DIR is required to generate Elitea protocol bindings")?,
    );
    let platform_root = manifest_dir
        .parent()
        .and_then(|services| services.parent())
        .ok_or("the Rust worker must remain under elitea-platform/services")?;
    let proto_root = platform_root.join("libs/proto");
    let protos = [
        "elitea/config/v1/capability_manifest.proto",
        "elitea/runtime/v1/agent.proto",
        "elitea/runtime/v1/command.proto",
        "elitea/runtime/v1/common.proto",
        "elitea/runtime/v1/control.proto",
        "elitea/runtime/v1/envelope.proto",
        "elitea/runtime/v1/errors.proto",
        "elitea/runtime/v1/indexing.proto",
        "elitea/runtime/v1/input.proto",
        "elitea/runtime/v1/limits.proto",
        "elitea/runtime/v1/node_event.proto",
        "elitea/runtime/v1/output.proto",
        "elitea/runtime/v1/toolkit.proto",
        "elitea/runtime/v1/validation.proto",
    ]
    .map(|relative| proto_root.join(relative));

    let mut prost_config = tonic_prost_build::Config::new();
    prost_config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);

    println!("cargo:rerun-if-changed={}", proto_root.display());
    println!("cargo:rerun-if-changed=build.rs");
    for proto in &protos {
        println!("cargo:rerun-if-changed={}", proto.display());
    }

    tonic_prost_build::configure()
        .build_server(false)
        .include_file("elitea.rs")
        .compile_with_config(prost_config, &protos, &[proto_root])?;

    Ok(())
}
