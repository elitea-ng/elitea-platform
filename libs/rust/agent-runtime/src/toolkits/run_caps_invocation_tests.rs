//! The run caps at the tool call boundary (ADR-0041 decision 3).

use std::collections::BTreeMap;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};

use adk_core::{ErrorComponent, Tool, ToolContext};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use serde_json::{Value, json};

use super::invocation::policy_bound_tool_for_test;
use super::policy::ToolAdmissionPolicy;
use crate::run_caps::{RunCap, RunCapLimits, RunCapsMeter, report_records};

/// A tool that returns `records` records and reports them.
struct RecordsTool {
    records: u64,
    calls: AtomicUsize,
}

#[async_trait]
impl Tool for RecordsTool {
    fn name(&self) -> &'static str {
        "search"
    }

    fn description(&self) -> &'static str {
        "fixture search"
    }

    async fn execute(&self, _: Arc<dyn ToolContext>, _: Value) -> adk_core::Result<Value> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        report_records(self.records);
        Ok(json!({"results": self.records}))
    }
}

fn bound(records: u64, meter: &Arc<RunCapsMeter>) -> (Arc<RecordsTool>, Arc<dyn Tool>) {
    let inner = Arc::new(RecordsTool {
        records,
        calls: AtomicUsize::new(0),
    });
    let policy = ToolAdmissionPolicy::new(&[], &BTreeMap::<String, Vec<String>>::new())
        .expect("fixture policy")
        .with_run_caps(Arc::clone(meter));
    let tool = policy_bound_tool_for_test(inner.clone(), "jira", Arc::new(policy))
        .expect("policy-bound fixture tool");
    (inner, tool)
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(SimpleToolContext::new("run-caps-test").with_function_call_id("call-1"))
}

#[tokio::test]
async fn the_call_past_the_tool_call_cap_does_not_run() {
    let meter = RunCapsMeter::new(RunCapLimits {
        max_tool_calls: 2,
        ..RunCapLimits::unlimited()
    });
    let (inner, tool) = bound(0, &meter);
    tool.execute(context(), json!({}))
        .await
        .expect("first call");
    tool.execute(context(), json!({}))
        .await
        .expect("second call");
    let error = tool
        .execute(context(), json!({}))
        .await
        .expect_err("the third call is past the cap");
    assert_eq!(error.code, "run_caps.max_tool_calls");
    assert_eq!(error.component, ErrorComponent::Guardrail);
    assert_eq!(inner.calls.load(Ordering::SeqCst), 2);
    assert_eq!(meter.tripped(), Some(RunCap::MaxToolCalls));
}

#[tokio::test]
async fn records_past_the_cap_do_not_reach_the_model() {
    let meter = RunCapsMeter::new(RunCapLimits {
        max_records_read: 5,
        ..RunCapLimits::unlimited()
    });
    let (inner, tool) = bound(3, &meter);
    tool.execute(context(), json!({})).await.expect("3 of 5");
    let error = tool
        .execute(context(), json!({}))
        .await
        .expect_err("6 of 5 is past the cap");
    assert_eq!(error.code, "run_caps.max_records_read");
    assert_eq!(meter.records_read(), 6);
    // Every later call is refused before it runs.
    let error = tool
        .execute(context(), json!({}))
        .await
        .expect_err("the run reached a cap");
    assert_eq!(error.code, "run_caps.max_records_read");
    assert_eq!(inner.calls.load(Ordering::SeqCst), 2);
}

#[tokio::test]
async fn a_policy_without_a_meter_has_no_cap() {
    let policy = Arc::new(
        ToolAdmissionPolicy::new(&[], &BTreeMap::<String, Vec<String>>::new())
            .expect("fixture policy"),
    );
    assert_eq!(policy.run_caps().limits(), RunCapLimits::unlimited());
    let inner = Arc::new(RecordsTool {
        records: 1_000,
        calls: AtomicUsize::new(0),
    });
    let tool = policy_bound_tool_for_test(inner.clone(), "jira", policy)
        .expect("policy-bound fixture tool");
    for _ in 0..250 {
        tool.execute(context(), json!({})).await.expect("no cap");
    }
    assert_eq!(inner.calls.load(Ordering::SeqCst), 250);
}
