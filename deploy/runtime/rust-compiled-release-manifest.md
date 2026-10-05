# Rust compiled release manifest

The producer creates one candidate profile for Main revision 1.
It does not enable the cache or publish runtime artifacts.
It does not create, hydrate, dispatch or remove a container.
Root owns those operations and the acceptance evidence.

## Public evidence boundary

Use the final shipping image and its exact retained native Cargo bundle.
Use an independently inspected immutable image ID.
Use the backend image digest from the trusted release process.
For Docker, both values can be the same inspected local image ID.
For Kubernetes, the release digest can be a measured registry manifest digest.
The audit accepts only the inspected local image ID or a measured immutable RepoDigest.
It reads digest portions only, without registry URLs or image environment values.

Create a dedicated non-root container for inert native hydration.
Use network `none`, a readonly root, no capabilities and `no-new-privileges`.
Set finite memory, swap, CPU and PID limits.
Use only bounded owner-private `/workspace` and optional `/tmp` tmpfs mounts.
Use `/bin/sh -c 'exec sleep 600'` as the inert entrypoint.
Import the retained bundle through the existing production native helper.
Wait for its original ready metadata and profile marker.
Keep compilation and user execution disabled.
Do not use a live deployment container.
Do not mount host files, sockets, credentials or instrumented compiler overlays.

The audit rejects compiled launch fields and incompatible container policy.
Use only unique `rw,nosuid,nodev,size,uid,gid,mode` tmpfs options.
The audit permits the optional valueless `exec` option.
Use `uid=10001,gid=10001,mode=0700` and one positive bounded `size` value.
Use lowercase `k`, `m` or `g` units without leading zeros.
The audit rejects duplicate keys, conflicting flags and unknown options.
It reads only fixed native, profile, adapter and toolchain paths.
It verifies the bundle root, record pin, compressed archive and actual hydrated tree.
It rejects links, special files, unknown content and changed readback.
It preserves the runner's exact vendor, toolchain and compiler configuration hash domains.
The native compiler configuration binds its explicit target and fixed two-job setting.
The command output, file count, byte count and total audit time have fixed bounds.
The total audit limit is 600 seconds.
Root must retain and clean up the original audit container after a command failure.

## Run the audit

Use host Python 3.12 or later.
Populate these variables from reviewed public release evidence.
Do not copy values from synthetic fixtures or instrumented probes.

```sh
python3 scripts/runtime/rust_compiled_release_manifest.py audit-native \
  --container-id "$AUDIT_CONTAINER_ID" \
  --image-id "$RELEASE_IMAGE_ID" \
  --image-digest "$RELEASE_IMAGE_DIGEST" \
  --bundle-sha256 "$NATIVE_PACKAGE_ROOT" \
  --policy-revision "$CACHE_POLICY_REVISION" \
  --output "$AUDIT_FILE"
```

Use a new absolute canonical output path in an operator-controlled directory.
The script reports the exact audit SHA-256 and byte count.
Review the audit against the independent image inspection and original inert hydration evidence.
Pin the exact reviewed bytes.
A byte pin does not establish the origin of fabricated evidence.
The operator must establish that origin before using the profile.
The schema cannot distinguish a fabricated audit from a trusted audit.

## Produce the candidate

Run the producer without a container connection.
Set `AUDIT_SHA256` to the reviewed external pin.

```sh
python3 scripts/runtime/rust_compiled_release_manifest.py produce \
  --audit "$AUDIT_FILE" \
  --audit-sha256 "$AUDIT_SHA256" \
  --image-id "$RELEASE_IMAGE_ID" \
  --image-digest "$RELEASE_IMAGE_DIGEST" \
  --bundle-sha256 "$NATIVE_PACKAGE_ROOT" \
  --policy-revision "$CACHE_POLICY_REVISION" \
  --output "$CANDIDATE_PROFILES_FILE"
```

The producer rejects a mutable tag, wrong cohort, changed pin or malformed audit.
It rejects an ordinary request binding or a profile file used as an audit.
It writes exact canonical JSON without a trailing newline.
It keeps the 19 Binding fields in the existing owner order.
It uses neutral scope and request template fields.
Main replaces those fields with verified original execution authority.
The audit supplies no execution or publication grant.

## Acceptance and deployment

Keep cache startup off until the native cohort passes isolated cold and warm acceptance.
Install the candidate only in that isolated cohort first.
Use the existing Main, Worker and Supervisor startup settings.
Mirror exact profile bytes and the same SHA pin through readonly material mounts.
Use non-executable `0444` or `0644` public files.
Keep private receipt database material separate.

The cache exports one executable and one descriptor.
It does not export build-generated companions, shared libraries or a target directory.
The inert audit proves content identity, not executable runtime eligibility.
Do not infer eligibility from archive validation or an operator assertion.
Prove the actual native cohort in distinct cold and warm execution sandboxes.
Verify its exact state and required runtime closure.
Refuse the cohort if it requires companions outside the exact image or bundle.
Keep general companion support deferred and off.
A general eligibility check must exist before broader cache enablement.
No such check or new authority layer exists in this producer.

Run `python3 -m unittest -v test_rust_compiled_release_manifest` from `scripts/runtime`.
The tests use synthetic data and never run Docker.
Do not treat these tests as deployed, Linux, native cohort or performance evidence.
