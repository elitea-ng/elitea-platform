# Sandbox code runner

This independent Rust crate builds the small trusted container-main-process
runner. It does not import the worker, Docker client, database, or platform
credentials. Keep this ownership boundary: the supervisor owns admission and
receipts; the container runtime enforces CPU/memory/PID/network/filesystem limits.

Use locked Cargo dependencies. Test with cargo test --locked.
Bound request bytes, captured output and wall time before retaining data.
Treat child output as untrusted. Never log request bodies or inherited secrets.
Linux-container tests must prove descendant termination; local unit tests do not.

The worker source-mapping history remains at
../elitea-worker-rust/docs/source-mapping/code-node-isolation-assessment-20260928.md.
Document integration and verification limits there.
