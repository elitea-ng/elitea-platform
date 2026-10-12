use std::env;
use std::error::Error;
use std::path::PathBuf;

fn main() -> Result<(), Box<dyn Error>> {
    let manifest_dir = PathBuf::from(
        env::var_os("CARGO_MANIFEST_DIR")
            .ok_or("CARGO_MANIFEST_DIR is required to generate the vector protocol bindings")?,
    );
    let platform_root = manifest_dir
        .parent()
        .and_then(|services| services.parent())
        .ok_or("elitea-vector must remain under elitea-platform/services")?;
    let proto_root = platform_root.join("libs/proto");
    let protos = [
        "elitea/vector/v1/vector.proto",
        "elitea/vector/v1/introspection.proto",
    ]
    .map(|relative| proto_root.join(relative));

    let mut prost_config = tonic_prost_build::Config::new();
    prost_config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);

    println!("cargo:rerun-if-changed=build.rs");
    for proto in &protos {
        println!("cargo:rerun-if-changed={}", proto.display());
    }

    // Both sides of both services: the server and the introspection client
    // ship; the vector client and the introspection server serve the tests.
    tonic_prost_build::configure()
        .build_server(true)
        .build_client(true)
        .include_file("elitea.rs")
        .compile_with_config(prost_config, &protos, &[proto_root])?;

    Ok(())
}
