# elitea-sandbox-runtime

An optional chart that installs **gVisor** on a cluster's sandbox nodes, so that
the EliteA cloud executor (ADR-0034) can run workspace Pods under the `gvisor`
RuntimeClass. The `elitea` chart never touches nodes. This chart is the only
part that does, and you install it separately and on purpose.

You need it only on clusters whose platform provides no sandbox runtime:

| Platform | Sandbox runtime | This chart |
|---|---|---|
| GKE | A GKE Sandbox node pool (`--sandbox type=gvisor`). RuntimeClass `gvisor` | not needed |
| AKS | A Pod Sandboxing node pool (`--workload-runtime KataVmIsolation`). RuntimeClass `kata-vm-isolation` | not needed |
| EKS (AL2023) | none | `mode: containerd` |
| k3s | none | `mode: k3s` (the default) |

The full procedure, from node labels to a verified cloud turn, is in
[Cloud executor on Kubernetes](../../../apps/elitea-web/src/entries/docs/content/deployment/cloud-executor-kubernetes.mdx) (served at `/docs/deployment/cloud-executor-kubernetes`).

## What it does

A DaemonSet runs **only** on nodes that match `nodeSelector` (default
`elitea.ai/sandbox=true`) and tolerates their taint. Its pods run privileged,
with `hostPID` and the host's `/` mounted at `/host`.

1. **Binaries.** It downloads the pinned `gvisor.release`
   (`gvisor.tar.bz2` for the node's architecture). The archive's SHA-512 must
   match both the value pinned in `gvisor.sha512.<arch>` and the release's
   published `gvisor.tar.bz2.sha512`. It then installs only `runsc`,
   `containerd-shim-runsc-v1` and the `gvisor-bin/` sidecars that runsc
   starts sandboxes with (`gvisor_sentry` and the rest; recent runsc refuses
   to start a sandbox without them) into `/usr/local/bin`. If the installed
   binaries already hash to that release, nothing is downloaded. The chart
   never uses `latest` or `curl | sh`.
2. **Runtime registration.** The steps depend on `mode`:
   - `k3s`: the installer writes k3s's containerd template from k3s's own base
     template (`{{ template "base" . }}`) plus a marked `runsc` runtime block.
     The file is `config-v3.toml.tmpl` on containerd 2.0 (config version 3)
     and `config.toml.tmpl` on containerd 1.7, or the template that already
     exists. The operator's own content in that file is kept.
   - `containerd` (EKS AL2023, generic nodes), config version 3
     (containerd 2.x, whose imports merge plugin sections per key): the
     installer writes the drop-in `/etc/containerd/config.d/50-elitea-runsc.toml`.
     If `config.toml` has no `imports`, it adds
     `imports = ["/etc/containerd/config.d/*.toml"]` to it. Config version 2
     (containerd 1.7) replaces a whole plugin table from an import, which
     would drop a customised cri section (`SystemdCgroup`, `sandbox_image`),
     so there the installer appends only a marked
     `[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]` block
     to `config.toml` and uses no drop-in. Before any
     restart it validates with `containerd config dump`: the merged config
     must load, must register `runsc`, and must keep every setting it had
     before. Otherwise everything is rolled back and the pod fails.
3. **Restart.** The installer restarts `k3s-agent` (`k3s` on server nodes) or
   `containerd` through `nsenter` into PID 1, **exactly once per change**,
   and not at all when the config is already current. It backs up the
   previous file in `/var/lib/elitea-sandbox-runtime/backup`. If the unit
   does not come back healthy within `restartTimeoutSeconds` (one deadline
   for the whole wait), the installer restores the backup and restarts the
   unit again, waiting at most `restartTimeoutSeconds` more.
4. **Readiness.** The pod is Ready only after `runsc --version` answers on the
   host and the running containerd lists the `runsc` runtime (`k3s crictl
   info`, or `crictl info`). Without crictl, the check uses the effective
   config plus a restart that happened after the write.
5. **RuntimeClass.** It creates `RuntimeClass gvisor` with handler `runsc`,
   and with `scheduling.nodeSelector`/`tolerations` equal to the installer's.
   Every Pod with `runtimeClassName: gvisor` then lands on a sandbox node.
   The elitea supervisor reads this RuntimeClass and refuses any handler
   except `runsc`, `gvisor` or `kata*`.

The DaemonSet rolls out one node at a time (`maxUnavailable: 1`). A new script,
release or mode reruns the installer through the pod template's checksums.

## Install

```bash
# Label and taint the sandbox nodes.
kubectl label node <node> elitea.ai/sandbox=true
kubectl taint node <node> elitea.ai/sandbox=true:NoSchedule

# A namespace that admits privileged pods.
kubectl create namespace elitea-sandbox-runtime
kubectl label namespace elitea-sandbox-runtime pod-security.kubernetes.io/enforce=privileged

helm install sandbox-runtime deploy/helm/elitea-sandbox-runtime \
  -n elitea-sandbox-runtime            # k3s
# EKS AL2023:  --set mode=containerd

kubectl -n elitea-sandbox-runtime rollout status ds/sandbox-runtime-installer
kubectl get runtimeclass gvisor
```

Then set `sandboxKubernetes.runtimeClass.name: gvisor` and the same
`nodeSelector` and tolerations under `sandboxKubernetes.workspaceRuns` in the
`elitea` values.

## Values

| Key | Default | |
|---|---|---|
| `mode` | `k3s` | `k3s` or `containerd` |
| `nodeSelector` | `{elitea.ai/sandbox: "true"}` | Required. Selects the sandbox nodes, for the installer and for the RuntimeClass |
| `tolerations` | the `elitea.ai/sandbox=true:NoSchedule` taint | The installer's tolerations, and the RuntimeClass's |
| `gvisor.release` | `20261005.0` | A dated release |
| `gvisor.sha512.{x86_64,aarch64}` | the published values | Checksum of `gvisor.tar.bz2`. Change it together with `release` |
| `k3s.service` | detected | `k3s` or `k3s-agent` |
| `containerd.config`, `containerd.dropinDir`, `containerd.patchImports` | `/etc/containerd/config.toml`, `/etc/containerd/config.d`, `true` | containerd mode |
| `restartTimeoutSeconds` | `180` | Time to wait before a rollback |
| `runtimeClass.create`, `runtimeClass.name`, `runtimeClass.overhead` | `true`, `gvisor`, `{}` | |
| `image` | `python:3.12.14-slim-trixie` by digest | Debian 13 with bash, coreutils, util-linux (nsenter) and Python |
| `kata` | `{}` | Placeholder only. For Kata, use AKS Pod Sandboxing or kata-deploy |

## Upgrading gVisor

Pick a dated release from
`https://storage.googleapis.com/gvisor/releases/release/`. Read both
`<release>/{x86_64,aarch64}/gvisor.tar.bz2.sha512` and set them with
`gvisor.release`. Running sandboxes keep the binary they started with. New
Pods use the new one. Binary updates alone do not restart containerd.

## Removing

`helm uninstall` removes the DaemonSet and the RuntimeClass. It leaves the
binaries and the containerd config on the nodes. To remove those, delete the
marked block (or the drop-in) and `/usr/local/bin/{runsc,containerd-shim-runsc-v1}`,
then restart k3s or containerd. Backups are in
`/var/lib/elitea-sandbox-runtime/backup`.

## Tests

- `deploy/helm/tests/render-sandbox-runtime.sh`: the rendered objects and the
  guards.
- `deploy/helm/tests/sandbox-runtime-installer-test.sh`: `install.sh` and
  `check.sh` against a fake host. It covers both modes, idempotency, one
  restart per change, rollback, and checksum, `latest` and imports refusals.
