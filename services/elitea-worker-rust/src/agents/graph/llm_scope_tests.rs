use super::*;
use adk_rust::graph::ExecutionConfig;
use std::sync::atomic::AtomicUsize;

#[test]
fn graph_model_identity_is_stable_for_replacement_and_separate_for_visits_and_parents() {
    let definition =
        LlmNodeDefinition::from_yaml("id: answer\ntype: llm\ntransition: END\n").unwrap();
    let context = |thread, step| NodeContext::new(State::new(), ExecutionConfig::new(thread), step);
    let first = PipelineModelScope::for_node(&definition, &context("thread", 2), None).unwrap();
    let replacement =
        PipelineModelScope::for_node(&definition, &context("thread", 2), None).unwrap();
    assert_eq!(first.identity(), replacement.identity());
    for (thread, step) in [("thread", 3), ("sibling/thread", 2)] {
        assert_ne!(
            first.identity(),
            PipelineModelScope::for_node(&definition, &context(thread, step), None)
                .unwrap()
                .identity()
        );
    }
    let left = PipelineNodeEventScope::new("pipeline:child:2", "child", "thread/child").unwrap();
    let right = PipelineNodeEventScope::new("pipeline:child:4", "child", "thread/child").unwrap();
    assert_ne!(
        PipelineModelScope::for_node(&definition, &context("thread/child", 0), Some(&left))
            .unwrap()
            .identity(),
        PipelineModelScope::for_node(&definition, &context("thread/child", 0), Some(&right))
            .unwrap()
            .identity()
    );
    let changed = LlmNodeDefinition::from_yaml("id: other\ntype: llm\ntransition: END\n").unwrap();
    assert_ne!(
        first.identity(),
        PipelineModelScope::for_node(&changed, &context("thread", 2), None)
            .unwrap()
            .identity()
    );
}

#[derive(Default)]
struct Delegate(AtomicUsize);
#[async_trait]
impl Llm for Delegate {
    fn name(&self) -> &'static str {
        "fixture"
    }
    async fn generate_content(
        &self,
        _: LlmRequest,
        _: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        self.0.fetch_add(1, Ordering::SeqCst);
        Ok(Box::pin(stream::empty()))
    }
}

#[tokio::test]
async fn replay_requires_the_exact_result_then_allows_later_compacted_history() {
    let delegate = Arc::new(Delegate::default());
    let pending: Content = serde_json::from_value(
        json!({"role":"model","parts":[{"name":"lookup","id":"call-one","args":{}}]}),
    )
    .unwrap();
    let model = PipelineLlmReplayModel {
        delegate: delegate.clone(),
        state: AtomicU8::new(PIPELINE_REPLAY_PENDING),
        pending_content: pending.clone(),
    };
    let mut request: LlmRequest = serde_json::from_value(
        json!({"model":"fixture","contents":[{"role":"user","parts":[{"text":"Task"}]}]}),
    )
    .unwrap();
    request
        .tools
        .insert("lookup".into(), json!({"type":"object"}));
    let first = model
        .generate_content(request.clone(), false)
        .await
        .unwrap()
        .collect::<Vec<_>>()
        .await;
    assert_eq!(first.len(), 1);
    assert_eq!(delegate.0.load(Ordering::SeqCst), 0);
    assert!(
        model
            .generate_content(request.clone(), false)
            .await
            .is_err()
    );
    assert_eq!(delegate.0.load(Ordering::SeqCst), 0);
    request.contents.push(pending);
    request.contents.push(serde_json::from_value(json!({"role":"function","parts":[{"id":"call-one","functionResponse":{"name":"lookup","response":{"result":"verified"}}}]})).unwrap());
    drop(
        model
            .generate_content(request.clone(), false)
            .await
            .unwrap(),
    );
    request.contents = vec![
        Content::new("user").with_text("Task"),
        Content::new("model").with_text("Structured summary of prior verified work"),
    ];
    drop(model.generate_content(request, false).await.unwrap());
    assert_eq!(delegate.0.load(Ordering::SeqCst), 2);
}
