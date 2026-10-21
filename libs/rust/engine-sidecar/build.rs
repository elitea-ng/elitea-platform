//! The elitea.engine.v1 bindings (libs/proto/elitea/engine/v1/engine.proto).
//!
//! They serve the `server` feature — the service, its client and the
//! container probe. Without it this crate is the socket binder and the cgroup
//! reader, and its build needs neither protoc nor libs/proto (the document
//! extractor builds it that way).

fn main() -> Result<(), Box<dyn std::error::Error>> {
    println!("cargo:rerun-if-changed=build.rs");
    #[cfg(feature = "server")]
    {
        use std::env;
        use std::path::PathBuf;

        let manifest_dir = PathBuf::from(
            env::var_os("CARGO_MANIFEST_DIR")
                .ok_or("CARGO_MANIFEST_DIR is required to generate the engine protocol bindings")?,
        );
        let proto_root = manifest_dir
            .parent()
            .and_then(|libs| libs.parent())
            .ok_or("engine-sidecar must remain under elitea-platform/libs/rust")?
            .join("proto");
        let protos = ["elitea/engine/v1/engine.proto"].map(|relative| proto_root.join(relative));

        let mut prost_config = tonic_prost_build::Config::new();
        prost_config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);
        // The arguments and the result are JSON documents of up to tens of
        // MiB (Inventory's graphs): `bytes::Bytes`, not a Vec copy.
        prost_config.bytes(["."]);

        for proto in &protos {
            println!("cargo:rerun-if-changed={}", proto.display());
        }
        // The server ships in every engine; the client serves the container
        // probe (`healthcheck`) and the engines' tests.
        tonic_prost_build::configure()
            .build_server(true)
            .build_client(true)
            .compile_with_config(prost_config, &protos, &[proto_root])?;
    }
    Ok(())
}
