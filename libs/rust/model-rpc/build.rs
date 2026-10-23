//! The elitea.llm.v1 bindings (libs/proto/elitea/llm/v1/model.proto): the
//! client always, the server for the `testing` feature.

fn main() -> Result<(), Box<dyn std::error::Error>> {
    use std::env;
    use std::path::PathBuf;

    let manifest_dir = PathBuf::from(
        env::var_os("CARGO_MANIFEST_DIR")
            .ok_or("CARGO_MANIFEST_DIR is required to generate the model protocol bindings")?,
    );
    let proto_root = manifest_dir
        .parent()
        .and_then(|libs| libs.parent())
        .ok_or("model-rpc must remain under elitea-platform/libs/rust")?
        .join("proto");
    let proto = proto_root.join("elitea/llm/v1/model.proto");
    println!("cargo:rerun-if-changed=build.rs");
    println!("cargo:rerun-if-changed={}", proto.display());

    let mut config = tonic_prost_build::Config::new();
    config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);
    // Bodies as `bytes::Bytes`: a stream's chunks are handed on without a copy.
    config.bytes(["."]);
    tonic_prost_build::configure()
        .build_client(true)
        .build_server(env::var_os("CARGO_FEATURE_TESTING").is_some())
        .compile_with_config(config, &[proto], &[proto_root])?;
    Ok(())
}
