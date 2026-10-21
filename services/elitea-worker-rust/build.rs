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
        "elitea/runtime/v1/compiled_code.proto",
        "elitea/runtime/v1/content.proto",
        "elitea/runtime/v1/envelope.proto",
        "elitea/runtime/v1/errors.proto",
        "elitea/runtime/v1/indexing.proto",
        "elitea/runtime/v1/input.proto",
        "elitea/runtime/v1/limits.proto",
        "elitea/runtime/v1/node_event.proto",
        "elitea/runtime/v1/output.proto",
        "elitea/runtime/v1/sandbox.proto",
        "elitea/runtime/v1/toolkit.proto",
        "elitea/runtime/v1/validation.proto",
        "elitea/runtime/v1/workspace.proto",
        "elitea/runtime/v1/workspace_run.proto",
    ]
    .map(|relative| proto_root.join(relative));

    let mut prost_config = tonic_prost_build::Config::new();
    prost_config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);
    // The index result is the largest output payload by far (920 of a 1440-byte
    // frame inline). Boxing it keeps every frame, and every future that holds
    // one, small enough for the bounded HITL stack the output tests pin.
    prost_config.boxed(".elitea.runtime.v1.ExecutionOutputFrameV1.payload.index_ingest");

    println!("cargo:rerun-if-changed={}", proto_root.display());
    println!("cargo:rerun-if-changed=build.rs");
    for proto in &protos {
        println!("cargo:rerun-if-changed={}", proto.display());
    }

    // Server stubs exist for the supervisor and for unit tests only: the
    // worker's own tests stand up real in-process servers (a fake Main
    // content listener, a fake store), and a default worker build carries no
    // server code at all. A build script cannot see cfg(test), so the stubs
    // are always generated and the attribute removes them everywhere else.
    tonic_prost_build::configure()
        .build_server(true)
        .server_mod_attribute(".", r#"#[cfg(any(test, feature = "sandbox-supervisor"))]"#)
        .include_file("elitea.rs")
        .compile_with_config(prost_config, &protos, std::slice::from_ref(&proto_root))?;

    // The document extractor sidecar's contract (elitea.extract.v1), compiled
    // on its own so the include file above stays the runtime protocol's. The
    // worker is its client; the server side exists for the indexing tests'
    // fake extractor over a real Unix socket.
    let extract = proto_root.join("elitea/extract/v1/extract.proto");
    println!("cargo:rerun-if-changed={}", extract.display());
    let mut extract_config = tonic_prost_build::Config::new();
    extract_config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);
    // The document chunks as `bytes::Bytes`: the client streams slices of
    // one buffer instead of a copy per chunk (transport/extractor_client.rs).
    extract_config.bytes(["."]);
    tonic_prost_build::configure()
        .build_server(true)
        .build_client(true)
        .compile_with_config(extract_config, &[extract], &[proto_root])?;

    Ok(())
}
