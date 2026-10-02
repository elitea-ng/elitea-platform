# Code memory-limit acceptance

Use this fixture only in an isolated rehearsal with an enforced memory limit below 768 MiB.
The first JavaScript node touches bounded allocations until the container reaches its limit.
The second node must not execute. No provider call is required.

Verify the following evidence together:

- The original execution Pod reports `OOMKilled`.
- The supervisor persists `sandbox.memory_limit` as a failed receipt.
- The pipeline stops without creating the downstream sandbox job.
- The browser shows a Code-node failure with guidance and a support reference.
- Reload preserves the failure in persistent chat.
- Cleanup removes the terminated execution Pod after receipt persistence.

A configured Kubernetes memory limit alone does not prove this behavior.
The ignored Rust adapter test covers receipt interpretation and repeated reads.
It does not replace supervisor persistence, graph suppression, or browser acceptance.
