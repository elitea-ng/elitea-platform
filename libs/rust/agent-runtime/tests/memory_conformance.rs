//! The host-neutral memory conformance fixture (#1233): a scripted model
//! calls the tools of `fixtures/memory_conformance.json` in order, through
//! the adk agent loop, against an in-memory [`MemoryStore`]; every tool
//! result must equal the fixture's. The desktop runs the same fixture over
//! the platform's memory routes (`apps/elitea-desktop/src-tauri/src/d0/tests.rs`),
//! and the cloud worker will once its store exists (extraction stage 7).

use std::collections::HashMap;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use adk_agent::LlmAgentBuilder;
use adk_core::{Content, Llm, LlmRequest, LlmResponse, LlmResponseStream, Part};
use adk_runner::Runner;
use adk_session::{CreateRequest, InMemorySessionService, SessionService};
use async_trait::async_trait;
use elitea_agent_runtime::host::{
    HostError, HostErrorCode, MemoryEntry, MemoryRecall, MemoryStore,
};
use elitea_agent_runtime::memory_tools::MemoryTools;
use futures::StreamExt as _;
use serde_json::Value;

/// The fixture's calls: `(id, tool, args, result)`.
fn fixture() -> Vec<(String, String, Value, Value)> {
    let fixture: Value =
        serde_json::from_str(include_str!("fixtures/memory_conformance.json")).unwrap();
    fixture["calls"]
        .as_array()
        .unwrap()
        .iter()
        .map(|call| {
            (
                call["id"].as_str().unwrap().to_owned(),
                call["tool"].as_str().unwrap().to_owned(),
                call["args"].clone(),
                call["result"].clone(),
            )
        })
        .collect()
}

/// A store that starts empty and numbers entries from 1, as the platform's
/// does for a fresh user.
#[derive(Default)]
struct InMemoryStore(Mutex<Vec<MemoryEntry>>);

#[async_trait]
impl MemoryStore for InMemoryStore {
    async fn recall(&self, _input: &str) -> Result<MemoryRecall, HostError> {
        Ok(MemoryRecall::default())
    }

    async fn save(&self, content: &str) -> Result<MemoryEntry, HostError> {
        let mut entries = self.0.lock().unwrap();
        let entry = MemoryEntry {
            id: entries.len() as u64 + 1,
            content: content.to_owned(),
        };
        entries.push(entry.clone());
        Ok(entry)
    }

    async fn search(&self, query: &str, limit: u32) -> Result<Vec<MemoryEntry>, HostError> {
        let query = query.to_lowercase();
        Ok(self
            .0
            .lock()
            .unwrap()
            .iter()
            .filter(|entry| entry.content.to_lowercase().contains(&query))
            .take(limit as usize)
            .cloned()
            .collect())
    }

    async fn delete(&self, id: u64) -> Result<(), HostError> {
        let mut entries = self.0.lock().unwrap();
        let before = entries.len();
        entries.retain(|entry| entry.id != id);
        if entries.len() == before {
            return Err(HostError::new(HostErrorCode::NotFound, "no such memory"));
        }
        Ok(())
    }
}

/// Calls the fixture's tools one per step, then answers.
struct Scripted {
    calls: Vec<(String, String, Value, Value)>,
    step: AtomicUsize,
}

#[async_trait]
impl Llm for Scripted {
    fn name(&self) -> &'static str {
        "scripted"
    }

    async fn generate_content(
        &self,
        _request: LlmRequest,
        _stream: bool,
    ) -> adk_core::Result<LlmResponseStream> {
        let step = self.step.fetch_add(1, Ordering::SeqCst);
        let mut content = Content::new("model");
        match self.calls.get(step) {
            Some((id, tool, args, _)) => content.parts.push(Part::FunctionCall {
                name: tool.clone(),
                args: args.clone(),
                id: Some(id.clone()),
                thought_signature: None,
            }),
            None => content = content.with_text("Done."),
        }
        let response = LlmResponse {
            content: Some(content),
            turn_complete: true,
            ..LlmResponse::default()
        };
        Ok(Box::pin(futures::stream::iter([Ok(response)])))
    }
}

#[tokio::test]
async fn the_runtime_answers_the_memory_fixture() {
    let calls = fixture();
    let model = Arc::new(Scripted {
        calls: calls.clone(),
        step: AtomicUsize::new(0),
    });
    let memory = MemoryTools::new(Arc::new(InMemoryStore::default())).with_writes(true);
    let agent = LlmAgentBuilder::new("agent")
        .model(model)
        .toolset(memory.toolset())
        .build()
        .unwrap();
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: "app".to_owned(),
            user_id: "user".to_owned(),
            session_id: Some("s".to_owned()),
            state: HashMap::new(),
        })
        .await
        .unwrap();
    let runner = Runner::builder()
        .app_name("app")
        .agent(Arc::new(agent))
        .session_service(sessions)
        .build()
        .unwrap();
    let mut stream = runner
        .run_str("user", "s", Content::new("user").with_text("remember pnpm"))
        .await
        .unwrap();
    let mut results: HashMap<String, Value> = HashMap::new();
    while let Some(event) = stream.next().await {
        let event = event.unwrap();
        for part in event
            .content()
            .map(|c| c.parts.as_slice())
            .unwrap_or_default()
        {
            if let Part::FunctionResponse {
                function_response,
                id,
                ..
            } = part
            {
                results.insert(
                    id.clone().unwrap_or_default(),
                    function_response.response.clone(),
                );
            }
        }
    }
    for (id, tool, _, expected) in &calls {
        assert_eq!(results.get(id), Some(expected), "{tool} ({id})");
    }
}
