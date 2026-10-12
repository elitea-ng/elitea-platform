use std::env;
use std::error::Error;
use std::path::PathBuf;

fn main() -> Result<(), Box<dyn Error>> {
    let manifest_dir = PathBuf::from(
        env::var_os("CARGO_MANIFEST_DIR")
            .ok_or("CARGO_MANIFEST_DIR is required to generate the extract protocol bindings")?,
    );
    let platform_root = manifest_dir
        .parent()
        .and_then(|services| services.parent())
        .ok_or("elitea-doc-extractor must remain under elitea-platform/services")?;
    let proto_root = platform_root.join("libs/proto");
    let protos = ["elitea/extract/v1/extract.proto"].map(|relative| proto_root.join(relative));

    let mut prost_config = tonic_prost_build::Config::new();
    prost_config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);

    println!("cargo:rerun-if-changed=build.rs");
    for proto in &protos {
        println!("cargo:rerun-if-changed={}", proto.display());
    }

    // The server ships; the client serves `healthcheck` and the tests.
    tonic_prost_build::configure()
        .build_server(true)
        .build_client(true)
        .compile_with_config(prost_config, &protos, &[proto_root])?;

    Ok(())
}
