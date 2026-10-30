use std::collections::VecDeque;
use std::sync::Mutex;

use serde_json::json;

use super::*;
use crate::protocol::run_steps::command_identity;

/// Answers each call with the next scripted turn and keeps every request.
struct Scripted {
    turns: Mutex<VecDeque<Vec<LlmResponse>>>,
    requests: Mutex<Vec<LlmRequest>>,
}

impl Scripted {
    fn new(turns: Vec<Vec<LlmResponse>>) -> Arc<Self> {
        Arc::new(Self {
            turns: Mutex::new(turns.into()),
            requests: Mutex::new(Vec::new()),
        })
    }

    fn requests(&self) -> Vec<LlmRequest> {
        self.requests.lock().unwrap().clone()
    }
}

#[async_trait]
impl Llm for Scripted {
    fn name(&self) -> &'static str {
        "scripted"
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        _stream: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        self.requests.lock().unwrap().push(request);
        let turn = self
            .turns
            .lock()
            .unwrap()
            .pop_front()
            .ok_or_else(|| AdkError::agent("no scripted turn left"))?;
        Ok(replay(turn))
    }
}

/// A streamed answer: one partial delta per word, then the terminal.
fn answer(text: &str) -> Vec<LlmResponse> {
    let mut turn = text
        .split_inclusive(' ')
        .map(|word| LlmResponse {
            content: Some(Content::new("model").with_text(word)),
            partial: true,
            ..LlmResponse::default()
        })
        .collect::<Vec<_>>();
    turn.push(LlmResponse {
        finish_reason: Some(FinishReason::Stop),
        turn_complete: true,
        ..LlmResponse::default()
    });
    turn
}

fn tool_call() -> Vec<LlmResponse> {
    vec![LlmResponse {
        content: Some(Content {
            role: "model".to_owned(),
            parts: vec![Part::FunctionCall {
                name: "lookup".to_owned(),
                args: json!({}),
                id: Some("call_1".to_owned()),
                thought_signature: None,
            }],
        }),
        finish_reason: Some(FinishReason::Stop),
        turn_complete: true,
        ..LlmResponse::default()
    }]
}

fn schema_contract() -> Arc<OutputContract> {
    Arc::new(
        OutputContract::from_value(Some(&json!({
            "type": "json_schema",
            "schema": {"type": "object", "required": ["answer"]}
        })))
        .unwrap()
        .unwrap(),
    )
}

fn model(inner: &Arc<Scripted>, execution_id: &str) -> OutputContractModel {
    OutputContractModel::new(
        Arc::clone(inner) as Arc<dyn Llm>,
        schema_contract(),
        None,
        ContractStepScope {
            execution_id: execution_id.to_owned(),
            node_id: "extract".to_owned(),
        },
    )
}

fn request() -> LlmRequest {
    LlmRequest {
        model: "scripted".to_owned(),
        contents: vec![Content::new("user").with_text("Extract the answer.")],
        config: None,
        tools: std::collections::HashMap::new(),
        previous_response_id: None,
    }
}

async fn text_of(stream: LlmResponseStream) -> String {
    collect(stream)
        .await
        .unwrap()
        .iter()
        .flat_map(|response| response.content.iter())
        .flat_map(|content| content.parts.iter())
        .filter_map(Part::text)
        .collect()
}

#[tokio::test]
async fn a_valid_answer_passes_through_once() {
    let inner = Scripted::new(vec![answer(r#"{"answer": "4"}"#)]);
    let text = text_of(
        model(&inner, "")
            .generate_content(request(), true)
            .await
            .unwrap(),
    )
    .await;
    assert_eq!(text, r#"{"answer": "4"}"#);
    assert_eq!(inner.requests().len(), 1);
}

#[tokio::test]
async fn an_invalid_answer_is_retried_once_with_the_error_as_feedback() {
    let inner = Scripted::new(vec![
        answer("The answer is four."),
        answer(r#"{"answer": "4"}"#),
    ]);
    let text = text_of(
        model(&inner, "")
            .generate_content(request(), true)
            .await
            .unwrap(),
    )
    .await;
    // The rejected prose never reaches the caller.
    assert_eq!(text, r#"{"answer": "4"}"#);
    let requests = inner.requests();
    assert_eq!(requests.len(), 2);
    let retry = &requests[1].contents;
    assert_eq!(retry.len(), 3);
    assert_eq!(retry[1].role, "model");
    assert_eq!(retry[1].parts[0].text(), Some("The answer is four."));
    assert_eq!(retry[2].role, "user");
    assert!(
        retry[2].parts[0]
            .text()
            .is_some_and(|feedback| feedback.contains("not a JSON document"))
    );
}

#[tokio::test]
async fn a_second_invalid_answer_is_the_typed_violation() {
    let inner = Scripted::new(vec![answer("prose"), answer("more prose")]);
    let Err(error) = model(&inner, "").generate_content(request(), true).await else {
        panic!("a second invalid answer must fail");
    };
    assert_eq!(
        error.code,
        elitea_agent_runtime::output_contract::VIOLATION_CODE
    );
    assert_eq!(inner.requests().len(), 2);
}

#[tokio::test]
async fn a_retry_that_calls_a_tool_makes_the_next_final_answer_the_last_attempt() {
    let inner = Scripted::new(vec![
        tool_call(),
        answer("prose"),
        tool_call(),
        answer("still prose"),
    ]);
    let model = model(&inner, "");
    // The tool turn passes as it is.
    assert!(model.generate_content(request(), true).await.is_ok());
    // The first invalid answer is retried; the retry calls a tool.
    assert!(model.generate_content(request(), true).await.is_ok());
    // The next invalid final answer is the last attempt.
    let Err(error) = model.generate_content(request(), true).await else {
        panic!("the retry was already spent");
    };
    assert_eq!(
        error.code,
        elitea_agent_runtime::output_contract::VIOLATION_CODE
    );
}

#[tokio::test]
async fn an_answer_cut_at_the_output_limit_is_left_to_the_continuation() {
    let mut cut = answer("{\"answer\":");
    cut.last_mut().unwrap().finish_reason = Some(FinishReason::MaxTokens);
    let inner = Scripted::new(vec![cut]);
    assert!(
        model(&inner, "")
            .generate_content(request(), true)
            .await
            .is_ok()
    );
    assert_eq!(inner.requests().len(), 1);
}

#[tokio::test]
async fn each_validation_is_a_step_record_without_the_answer() {
    let execution_id = "exec-output-contract-steps";
    let recorder =
        RunStepRegistry::global().recorder(&command_identity(execution_id, 1, "command-1"));
    let inner = Scripted::new(vec![answer("secret prose"), answer("secret again")]);
    let _ = model(&inner, execution_id)
        .generate_content(request(), true)
        .await;
    let steps = recorder.next_batch();
    assert_eq!(steps.len(), 2);
    for (index, step) in steps.iter().enumerate() {
        assert_eq!(step.kind, RunStepKindV1::Output as i32);
        assert_eq!(step.name, CHECK_STEP_NAME);
        assert_eq!(step.node_id, "extract");
        assert!(step.inputs.is_none() && step.outputs.is_none());
        let summary: Value = serde_json::from_str(&step.summary_json).unwrap();
        assert_eq!(summary["attempt"], json!(index + 1));
        assert_eq!(summary["valid"], json!(false));
        assert_eq!(summary["keyword"], json!("json"));
        assert_eq!(summary["final"], json!(index == 1));
        assert!(!step.summary_json.contains("secret"));
    }
    RunStepRegistry::global().forget(&command_identity(execution_id, 1, "command-1"));
}

#[tokio::test]
async fn an_agent_answer_cut_at_the_output_limit_is_retried_as_invalid() {
    let mut cut = answer("{\"answer\":");
    cut.last_mut().unwrap().finish_reason = Some(FinishReason::MaxTokens);
    let inner = Scripted::new(vec![cut, answer(r#"{"answer": "4"}"#)]);
    let agent = OutputContractModel::new(
        Arc::clone(&inner) as Arc<dyn Llm>,
        schema_contract(),
        None,
        ContractStepScope::default(),
    );
    let text = text_of(agent.generate_content(request(), true).await.unwrap()).await;
    assert_eq!(text, r#"{"answer": "4"}"#);
    let requests = inner.requests();
    assert_eq!(requests.len(), 2);
    // The cut answer is not sent back; only the feedback is.
    assert_eq!(requests[1].contents.len(), 2);
    assert!(
        requests[1].contents[1].parts[0]
            .text()
            .is_some_and(|feedback| feedback.contains("output token limit"))
    );
}

fn continuation_request() -> LlmRequest {
    let mut continuation = request();
    continuation
        .contents
        .push(Content::new("user").with_text(
            "The previous answer reached its output allowance. Complete only the rest.",
        ));
    continuation
}

#[tokio::test]
async fn a_marked_continuation_call_of_a_node_is_not_validated() {
    let inner = Scripted::new(vec![answer(" \"4\"}")]);
    let text = text_of(
        continuation_call(model(&inner, "").generate_content(continuation_request(), true))
            .await
            .unwrap(),
    )
    .await;
    assert_eq!(text, " \"4\"}");
    assert_eq!(inner.requests().len(), 1);
}

/// Regression: the continuation prompt typed by an end user does not skip
/// the contract. Only internal state marks a continuation call.
#[tokio::test]
async fn the_continuation_prompt_in_user_text_is_validated() {
    let inner = Scripted::new(vec![answer("free prose"), answer("more free prose")]);
    let Err(error) = model(&inner, "")
        .generate_content(continuation_request(), true)
        .await
    else {
        panic!("user text must not skip the contract");
    };
    assert_eq!(
        error.code,
        elitea_agent_runtime::output_contract::VIOLATION_CODE
    );
    assert_eq!(inner.requests().len(), 2);
}

/// An agent never passes a cut answer through, so its answer is never
/// joined: a marked continuation call of an agent is a full answer to check.
#[tokio::test]
async fn a_marked_continuation_call_of_an_agent_is_validated() {
    let inner = Scripted::new(vec![answer(" \"4\"}"), answer(" \"4\"}")]);
    let agent = OutputContractModel::new(
        Arc::clone(&inner) as Arc<dyn Llm>,
        schema_contract(),
        None,
        ContractStepScope::default(),
    );
    let Err(error) = continuation_call(agent.generate_content(continuation_request(), true)).await
    else {
        panic!("an agent fragment must not skip the contract");
    };
    assert_eq!(
        error.code,
        elitea_agent_runtime::output_contract::VIOLATION_CODE
    );
}

/// Regression: the retry is per final answer, not per binding. After an
/// accepted answer, the next invalid answer gets its own retry and its
/// attempts are recorded as 1 and 2 again.
#[tokio::test]
async fn each_final_answer_gets_its_own_retry() {
    let execution_id = "exec-output-contract-retry-per-answer";
    let recorder =
        RunStepRegistry::global().recorder(&command_identity(execution_id, 1, "command-1"));
    let inner = Scripted::new(vec![
        answer("prose"),
        answer(r#"{"answer": "4"}"#),
        answer(r#"{"answer": "5"}"#),
        answer("prose again"),
        answer(r#"{"answer": "6"}"#),
    ]);
    let model = model(&inner, execution_id);
    for expected in [
        r#"{"answer": "4"}"#,
        r#"{"answer": "5"}"#,
        r#"{"answer": "6"}"#,
    ] {
        let text = text_of(model.generate_content(request(), true).await.unwrap()).await;
        assert_eq!(text, expected);
    }
    assert_eq!(inner.requests().len(), 5);
    let attempts: Vec<(Value, Value)> = recorder
        .next_batch()
        .iter()
        .map(|step| {
            let summary: Value = serde_json::from_str(&step.summary_json).unwrap();
            (summary["attempt"].clone(), summary["valid"].clone())
        })
        .collect();
    assert_eq!(
        attempts,
        vec![
            (json!(1), json!(false)),
            (json!(2), json!(true)),
            (json!(1), json!(true)),
            (json!(1), json!(false)),
            (json!(2), json!(true)),
        ]
    );
    RunStepRegistry::global().forget(&command_identity(execution_id, 1, "command-1"));
}
