# Code platform receipt trust packaging

## Source mapping

Main signs committed Code replies with its existing command Ed25519 signing key.
The runner verifies those replies before it sends reply bytes to Code.
The optional wrapper supplies the missing public trust asset.
The existing Containerfile and its default images stay unchanged.

| Existing source | Ownership and new packaging behavior |
| --- | --- |
| `services/elitea-main/internal/runtimecomposition/composition.go` | Main loads the command private key. Main checks its public key against the public verification keyring. |
| `services/elitea-main/internal/runtimecomposition/verification_keyring.go` | The public format uses `schema_version: elitea.runtime-ed25519-keyring.v1`. Each entry contains `key_id` and `public_key_base64`. |
| `services/elitea-main/internal/transport/runtimegrpc/control/code_platform_signature.go` | `SandboxGrantIssuer.SignCommittedCodeCall` signs with that command key and its exact key ID. |
| `services/elitea-main/internal/domain/codeplatform/signature.go` | The signing input contains the committed-reply domain, an unsigned 64-bit big-endian length, and exact claims bytes. |
| `services/elitea-code-runner/src/code_platform_signature.rs` | The runner verifies the same domain and framing with Ed25519. |
| `services/elitea-code-runner/src/code_platform_launch.rs` | The runner reads `/opt/elitea-code-trust/main-receipt-keys.json` before child creation. |
| `scripts/runtime/code_platform_public_trust.py` | The helper converts public base64 bytes to lowercase hex. It preserves each exact key ID. |
| `services/elitea-code-runner/Containerfile.code-platform` | The wrapper validates public trust and copies one asset. It retains user `10001:10001` and the inherited runtime command. |

The runner asset contains only `revision: 1` and `keys`.
Each entry contains only `key_id` and `public_key_hex`.
The asset permits one through eight keys and at most 8192 bytes.
Each public key contains 32 bytes. Each key ID contains one through 256 printable ASCII characters without spaces.

Main permits up to 64 public keys and broader key IDs.
The helper rejects incompatible input and duplicate public material, including aliases under different IDs.
Select required rotation keys explicitly when the Main keyring contains more than eight keys.
Include Main's active `config.SigningKeyID` in every output asset.
The helper rejects unknown selection IDs. It never truncates a keyring silently.

Main's `code-platform-content-keys.json` is a separate private AES content keyring.
Its owner remains Main. The converter rejects that format and every private or unknown field.
Never supply the command private PEM or content keyring to either build context.

## Opt-in preparation

Use a fresh operator-controlled directory for public trust.
Keep the public input keyring outside that directory.
Use absolute canonical paths without symlinks.
Do not use executable files, hard links, devices, FIFOs, or files writable by group or other users.

```sh
python3 -B scripts/runtime/code_platform_public_trust.py convert \
  --input /absolute/public-input/command-signing-keyring.json \
  --require-key-id main-signing-v1 \
  --output /absolute/public-trust/main-receipt-keys.json
```

Add one `--key-id` argument for each selected rotation key.
The helper validates the full public input before selection.
It creates the output exclusively with mode `0444`.
Its status contains only the output size, SHA-256, fixed path, and result.
The SHA-256 identifies the normalized output bytes.

Prepare a separate helper context containing only `code_platform_public_trust.py`.
Copy the helper from `scripts/runtime/code_platform_public_trust.py` into that context.
Keep the public trust context limited to `main-receipt-keys.json`.
Keep both contexts immutable during the build.
The helper checks file identity before and after reading.
Directory ownership remains an operator requirement; portable checks cannot prevent concurrent replacement of writable parent directories.

## Optional wrapper build

Use an admitted runner repository reference pinned by its actual registry manifest digest.
Set `CODE_PLATFORM_REQUIRED_KEY_ID` to Main's active signing key ID.
The validator refuses mutable tags and missing key IDs.

```sh
docker buildx build \
  --file services/elitea-code-runner/Containerfile.code-platform \
  --build-arg CODE_RUNNER_IMAGE=registry.example/elitea/code-runner@sha256:<manifest-digest> \
  --build-arg CODE_PLATFORM_REQUIRED_KEY_ID=main-signing-v1 \
  --build-context code-platform-trust=/absolute/public-trust \
  /absolute/helper-only-context
```

Replace the example with an actual immutable repository reference.
The validation stage uses the same pinned Python image as the existing runner verification stage.
The final image contains only the validated public asset at the fixed trust path.
The asset belongs to root and has mode `0444`.
The trust directory belongs to root and has mode `0755`.
The validation stage creates a dedicated directory with that explicit mode.
It writes only the validated public asset inside that directory.
The final stage copies the directory tree without a mode override.
This preserves directory traversal and the file's read-only mode.
The final stage inherits the base command, entrypoint, environment, and working directory.
It sets the existing runner user explicitly.
The validation interpreter and helper do not enter the final image.

Keep the inspected image ID and recorded `RepoDigests` as separate evidence fields.
Do not infer a configuration digest from `.Id`; its meaning depends on the Docker image store.
Root reports these actual read-back values from the current containerd-backed rehearsal engine:

| Candidate | Inspected image ID | Recorded repository digest |
| --- | --- | --- |
| Rust | `sha256:355653a4eaced3bdd74a57eb5df9a4655a1e1117158dcdb2168cc5a6a574ecfd` | `elitea-code-rust@sha256:355653a4eaced3bdd74a57eb5df9a4655a1e1117158dcdb2168cc5a6a574ecfd` |
| Deno | `sha256:b16da2d30ffb417e7b362337321ba6f587d049f6b37587aff8a83ebeeb04c6aa` | `elitea-code-deno@sha256:b16da2d30ffb417e7b362337321ba6f587d049f6b37587aff8a83ebeeb04c6aa` |

The matching hashes do not justify inventing repository references.
These are root-owned rehearsal values, not independently verified or published release references.
The validator rejects bare local image IDs.
Root must prove that its selected builder resolves the recorded repository reference before wrapper acceptance.
Root's current Docker-driver probe cannot resolve those unpublished repository references.
This is a local builder limitation. It does not change the public trust contract.
Root can test source-stage or OCI-context rehearsal builds separately.
Those methods do not prove resolution of the production wrapper's repository reference.

## Verification and limits

Run the focused source and fixture tests:

```sh
python3 -B scripts/runtime/test_code_platform_public_trust.py -v
```

The tests cover exact conversion, rotation selection, input bounds, duplicate fields, duplicate keys, and private-field refusal.
They cover malformed encodings, nesting bounds, safe regular files, file replacement, exclusive publication, and immutable image references.
They check the source signer, fixed runner asset contract, and optional wrapper instructions.
The public fixture contains an RFC 8032 public test key and no private signing material.
The helper follows existing `rust_compiled_release_manifest.py` serialization and publication patterns.
It remains independent because that script also contains unrelated Docker audit operations.
The helper adds nonblocking opens and a JSON nesting bound.

Root's container read-back found a parent-directory mode defect in the first wrapper.
Docker's file copy with `--chmod=0444` also created the destination parent with mode `0444`.
User `10001` could not traverse that directory or read the public asset.
The follow-up copies the dedicated directory tree and removes the final copy's mode override.
Focused checks cover the source instructions and retained directory and file modes.
Local directory-copy fixtures do not prove Docker copy behavior or user `10001` access.

These checks do not prove an image build or a live receipt exchange.
Root owns exact wrapper builds, asset read-back, file ownership, non-root execution, and runtime acceptance.
Root must record the final image identity and its trust asset hash.
Root must confirm that user source cannot replace the fixed asset.
Root must verify signed replies with the admitted public keys and reject unknown signing keys.
