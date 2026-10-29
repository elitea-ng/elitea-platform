//! The egress policy outside any run, as every consumer of this crate builds
//! it (#1490 review). An integration test, so the crate is compiled WITHOUT
//! `cfg(test)`: its own unit tests keep an open policy for clients built
//! directly against local fixtures, and could not see this.

use elitea_agent_runtime::egress::{RunEgress, current, scope};

#[tokio::test]
async fn a_client_built_outside_any_run_reaches_no_public_host() {
    let egress = current();
    assert!(!egress.policy().is_open());
    assert!(egress.check("api.example.com", 443).is_err());
}

#[tokio::test]
async fn a_task_spawned_inside_a_run_does_not_fall_back_to_an_open_policy() {
    // A task-local does not cross `tokio::spawn`: the spawned task is outside
    // the run, and must refuse rather than reach every host.
    let spawned = scope(RunEgress::open(), async {
        tokio::spawn(async { current() }).await.expect("task")
    })
    .await;
    assert!(!spawned.policy().is_open());
    assert!(spawned.check("api.example.com", 443).is_err());
    // The run in scope is still the run's own.
    let run = RunEgress::open();
    let inside = scope(std::sync::Arc::clone(&run), async { current() }).await;
    assert!(std::sync::Arc::ptr_eq(&inside, &run));
}
