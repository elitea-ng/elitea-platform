use std::env;
use std::error::Error;
use std::path::PathBuf;

fn main() -> Result<(), Box<dyn Error>> {
    let manifest_dir = PathBuf::from(
        env::var_os("CARGO_MANIFEST_DIR")
            .ok_or("CARGO_MANIFEST_DIR is required to generate the vector protocol bindings")?,
    );
    let proto_root = manifest_dir
        .parent()
        .and_then(|libs| libs.parent())
        .ok_or("vector-index must remain under elitea-platform/libs/rust")?
        .join("proto");
    let protos = ["elitea/vector/v1/vector.proto"].map(|relative| proto_root.join(relative));

    let mut prost_config = tonic_prost_build::Config::new();
    prost_config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);

    println!("cargo:rerun-if-changed=build.rs");
    for proto in &protos {
        println!("cargo:rerun-if-changed={}", proto.display());
    }

    // The client always ships; the server only serves the tests' fake.
    tonic_prost_build::configure()
        .build_server(env::var_os("CARGO_FEATURE_TEST_SERVER").is_some())
        .build_client(true)
        .include_file("elitea.rs")
        .compile_with_config(prost_config, &protos, &[proto_root])?;

    Ok(())
}
