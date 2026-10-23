//! The elitea.runtime.v1 artifact-object and internal-tool bindings
//! (`libs/proto/elitea/runtime/v1/artifact_object.proto`, `internal_tool.proto`):
//! the clients always, the servers for the `testing` feature.

fn main() -> Result<(), Box<dyn std::error::Error>> {
    use std::env;
    use std::path::PathBuf;

    let manifest_dir = PathBuf::from(
        env::var_os("CARGO_MANIFEST_DIR")
            .ok_or("CARGO_MANIFEST_DIR is required to generate the platform protocol bindings")?,
    );
    let proto_root = manifest_dir
        .parent()
        .and_then(|libs| libs.parent())
        .ok_or("platform-rpc must remain under elitea-platform/libs/rust")?
        .join("proto");
    let protos = [
        proto_root.join("elitea/runtime/v1/artifact_object.proto"),
        proto_root.join("elitea/runtime/v1/internal_tool.proto"),
    ];
    println!("cargo:rerun-if-changed=build.rs");
    for proto in &protos {
        println!("cargo:rerun-if-changed={}", proto.display());
    }

    let mut config = tonic_prost_build::Config::new();
    config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);
    // Object chunks and tool JSON as `bytes::Bytes`: handed on without a copy.
    config.bytes(["."]);
    tonic_prost_build::configure()
        .build_client(true)
        .build_server(env::var_os("CARGO_FEATURE_TESTING").is_some())
        .compile_with_config(config, &protos, &[proto_root])?;
    Ok(())
}
