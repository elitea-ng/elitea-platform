# Sandbox: code Pods verify the sandbox RuntimeClass before scheduling (#2968, ADR-0011)

**What changed.** Before the Kubernetes sandbox supervisor creates a code-node
Pod, it reads the code profile's `runtime_class` from the API server and
accepts it only when its handler is a sandbox runtime: `runsc` or `gvisor`
(gVisor) or `kata*` (Kata). Workspace-run and browser-session Pods already did
this. The verdict is cached the same way (a success for a minute, a refusal for
five seconds), so a fixed RuntimeClass is picked up without a restart.

A missing RuntimeClass, one the supervisor cannot read, or a `runc` or `crun`
handler now **refuses the code job** instead of running user code under the
standard container boundary. The job fails with "Kubernetes sandbox
RuntimeClass does not isolate code Pods", and the supervisor logs
`code_pod_runtime_class_refused` with the RuntimeClass name.

The chart now renders its `elitea-sandbox-runtimeclass-<namespace>`
ClusterRole and ClusterRoleBinding (`get` on `sandboxKubernetes.runtimeClass.name`
only) whenever `sandboxKubernetes.enabled` is true. Before, they rendered only
with workspace runs or browser sessions.

**Who is affected.** Deployments with `sandboxKubernetes.enabled: true`:

- whose cluster has no sandbox RuntimeClass, or one on `runc`;
- whose code profiles (the supervisor configuration Secret) name a
  `runtime_class` other than `sandboxKubernetes.runtimeClass.name`, because the
  supervisor may read only that one RuntimeClass;
- that install the chart with an identity that cannot create a ClusterRole.

**What to do.**

- Provide a sandbox RuntimeClass (GKE Sandbox `gvisor`, AKS
  `kata-vm-isolation`, or `sandboxRuntime.enabled` for EKS and k3s) and set
  `sandboxKubernetes.runtimeClass.name` to it (default `gvisor`).
- Set every code profile's `runtime_class` to that same name.
- Allow the installing identity to create the one ClusterRole and
  ClusterRoleBinding above.
- No new chart values.
