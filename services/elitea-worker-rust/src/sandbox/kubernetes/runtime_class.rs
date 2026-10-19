//! The isolation check on a workspace profile's `RuntimeClass` (ADR-0034).
//!
//! A workspace run executes the agent's `run_command` on the user's project,
//! so its Pod must run under a sandboxing runtime, never the node's default
//! runc. The profile only names a `RuntimeClass`, and a DNS-valid name proves
//! nothing about what it runs, so the supervisor reads the `RuntimeClass` from
//! the API server and accepts it only when its handler is gVisor (`runsc`,
//! or `gvisor` as GKE Sandbox names it) or Kata (`kata*`). It is fail-closed: a missing `RuntimeClass`, a read the
//! API server refuses (RBAC) or fails, and any other handler (`runc`,
//! `crun`, ...) all refuse.

use std::time::Duration;

use k8s_openapi::api::node::v1::RuntimeClass;
use kube::Api;

const API_TIMEOUT: Duration = Duration::from_secs(15);

/// Whether a `RuntimeClass` handler isolates a workload from the node kernel.
/// Everything else, `runc` and `crun` included, does not.
///
/// - `runsc`: gVisor as its own docs and the `elitea-sandbox-runtime` chart
///   register it (<https://gvisor.dev/docs/user_guide/containerd/quick_start/>).
/// - `gvisor`: GKE Sandbox. The `RuntimeClass` GKE creates is named `gvisor`
///   (<https://cloud.google.com/kubernetes-engine/docs/how-to/sandbox-pods>);
///   GKE does not document its handler, and its nodes register the containerd
///   runtime under that same name (runtime type `io.containerd.runsc.v1`).
/// - `kata*`: Kata Containers (`kata`, `kata-qemu`, `kata-clh`, `kata-fc`),
///   AKS Pod Sandboxing included: its `RuntimeClass` is `kata-vm-isolation`
///   (formerly `kata-mshv-vm-isolation`) on a Kata handler
///   (<https://learn.microsoft.com/azure/aks/use-pod-sandboxing>).
///
/// The handler names the node's containerd runtime, so this is a check on
/// what the cluster operator registered, not on the node itself.
#[must_use]
pub fn isolating_handler(handler: &str) -> bool {
    matches!(handler, "runsc" | "gvisor") || handler.starts_with("kata")
}

/// Why a `RuntimeClass` was refused. Each message names the `RuntimeClass` and
/// what an operator changes.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum RuntimeClassRefusal {
    #[error(
        "RuntimeClass {0:?} does not exist; create it with handler runsc or gvisor (gVisor) or kata* (Kata), e.g. with the elitea-sandbox-runtime chart, GKE Sandbox or AKS Pod Sandboxing"
    )]
    Missing(String),
    #[error(
        "RuntimeClass {name:?} could not be read ({detail}); the supervisor needs `get` on runtimeclasses.node.k8s.io and refuses workspace runs until it can verify the handler"
    )]
    Unreadable { name: String, detail: String },
    #[error(
        "RuntimeClass {name:?} uses handler {handler:?}, which does not isolate workspace runs; only runsc or gvisor (gVisor) and kata* (Kata) are accepted"
    )]
    NotIsolating { name: String, handler: String },
}

/// Read the `RuntimeClass` `name` and return its handler when it isolates.
///
/// # Errors
///
/// [`RuntimeClassRefusal`] in every case but an isolating handler.
pub async fn verify(client: kube::Client, name: &str) -> Result<String, RuntimeClassRefusal> {
    let classes: Api<RuntimeClass> = Api::all(client);
    let unreadable = |detail: String| RuntimeClassRefusal::Unreadable {
        name: name.to_owned(),
        detail,
    };
    let class = tokio::time::timeout(API_TIMEOUT, classes.get_opt(name))
        .await
        .map_err(|_| unreadable("timed out".into()))?
        .map_err(|error| match error {
            kube::Error::Api(status) => unreadable(format!("{} {}", status.code, status.reason)),
            _ => unreadable("API request failed".into()),
        })?
        .ok_or_else(|| RuntimeClassRefusal::Missing(name.to_owned()))?;
    if isolating_handler(&class.handler) {
        Ok(class.handler)
    } else {
        Err(RuntimeClassRefusal::NotIsolating {
            name: name.to_owned(),
            handler: class.handler,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn only_gvisor_and_kata_handlers_isolate() {
        for handler in [
            "runsc",
            "gvisor",
            "kata",
            "kata-qemu",
            "kata-fc",
            "kata-clh",
        ] {
            assert!(isolating_handler(handler), "{handler}");
        }
        for handler in [
            "runc",
            "crun",
            "",
            "runsc-debug",
            "RUNSC",
            "youki",
            "nvidia",
        ] {
            assert!(!isolating_handler(handler), "{handler}");
        }
    }

    fn response(
        status: u16,
        value: &serde_json::Value,
    ) -> http::Response<http_body_util::Full<bytes::Bytes>> {
        http::Response::builder()
            .status(status)
            .body(http_body_util::Full::new(bytes::Bytes::from(
                serde_json::to_vec(value).unwrap(),
            )))
            .unwrap()
    }

    /// A fake API server that answers one `RuntimeClass` read.
    fn api(status: u16, body: serde_json::Value) -> kube::Client {
        kube::Client::new(
            tower::service_fn(move |request: http::Request<kube::client::Body>| {
                assert_eq!(request.method(), http::Method::GET);
                assert_eq!(
                    request.uri().path(),
                    "/apis/node.k8s.io/v1/runtimeclasses/gvisor"
                );
                let body = body.clone();
                async move { Ok::<_, std::convert::Infallible>(response(status, &body)) }
            }),
            "default",
        )
    }

    fn class(handler: &str) -> serde_json::Value {
        json!({"apiVersion":"node.k8s.io/v1","kind":"RuntimeClass",
               "metadata":{"name":"gvisor"},"handler":handler})
    }

    fn status(code: u16, reason: &str) -> serde_json::Value {
        json!({"kind":"Status","apiVersion":"v1","status":"Failure",
               "reason":reason,"message":"test","code":code})
    }

    #[tokio::test]
    async fn an_isolating_runtime_class_is_accepted() {
        assert_eq!(
            verify(api(200, class("runsc")), "gvisor").await.unwrap(),
            "runsc"
        );
        assert_eq!(
            verify(api(200, class("kata-qemu")), "gvisor")
                .await
                .unwrap(),
            "kata-qemu"
        );
    }

    #[tokio::test]
    async fn runc_crun_missing_and_unreadable_all_refuse() {
        for handler in ["runc", "crun"] {
            assert_eq!(
                verify(api(200, class(handler)), "gvisor").await,
                Err(RuntimeClassRefusal::NotIsolating {
                    name: "gvisor".into(),
                    handler: handler.into()
                })
            );
        }
        assert_eq!(
            verify(api(404, status(404, "NotFound")), "gvisor").await,
            Err(RuntimeClassRefusal::Missing("gvisor".into()))
        );
        for (code, reason) in [(403, "Forbidden"), (500, "InternalError")] {
            let refusal = verify(api(code, status(code, reason)), "gvisor")
                .await
                .unwrap_err();
            assert!(
                matches!(&refusal, RuntimeClassRefusal::Unreadable { detail, .. } if detail.contains(reason)),
                "{refusal}"
            );
            assert!(refusal.to_string().contains("runtimeclasses.node.k8s.io"));
        }
    }
}
