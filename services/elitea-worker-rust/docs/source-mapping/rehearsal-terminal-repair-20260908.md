# Old-protocol rehearsal terminal repair

This operational branch starts at `046e84a2`.
It carries only the Rust source and tests from recovery fix `3da2c4e0`.
The source mapping for that fix remains on the main continuation branch.

The branch retains the original read-command and output field assignments.
No protobuf, migration, dependency, or runtime configuration changes occur here.
Do not merge this branch into main as a protocol update.

Build this branch only to settle existing old-format rehearsal work.
Preserve the current Main image, runtime secrets, output spool, and database.
Deploy only the replacement worker image.
Verify terminal publication, committed settlement, and Redis retirement before the schema cutover.

The focused encrypted-spool suite passes nine tests under the old protocol.
The complete current-protocol source mapping is `toolkit-terminal-recovery.md` at `3da2c4e0`.
It lists the recovery boundaries and the remaining cross-replica proofs.
