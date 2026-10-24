//! The memory tools over an in-memory store: what is offered, the guards
//! before a save, the error mapping and the approval path.

use std::sync::Mutex;
use std::sync::atomic::{AtomicBool, Ordering};

use adk_tool::SimpleToolContext;

use super::*;
use crate::host::MemoryRecall;

#[derive(Default)]
struct FakeStore {
    entries: Mutex<Vec<MemoryEntry>>,
    saves: Mutex<Vec<String>>,
}

#[async_trait]
impl MemoryStore for FakeStore {
    async fn recall(&self, _input: &str) -> Result<MemoryRecall, HostError> {
        Ok(MemoryRecall::default())
    }

    async fn save(&self, content: &str) -> Result<MemoryEntry, HostError> {
        self.saves.lock().unwrap().push(content.to_owned());
        let mut entries = self.entries.lock().unwrap();
        let entry = MemoryEntry {
            id: entries.len() as u64 + 1,
            content: content.to_owned(),
        };
        entries.push(entry.clone());
        Ok(entry)
    }

    async fn search(&self, query: &str, _limit: u32) -> Result<Vec<MemoryEntry>, HostError> {
        let query = query.to_lowercase();
        Ok(self
            .entries
            .lock()
            .unwrap()
            .iter()
            .filter(|entry| entry.content.to_lowercase().contains(&query))
            .cloned()
            .collect())
    }

    async fn get(&self, id: u64) -> Result<Option<MemoryEntry>, HostError> {
        Ok(self
            .entries
            .lock()
            .unwrap()
            .iter()
            .find(|entry| entry.id == id)
            .cloned())
    }

    async fn delete(&self, id: u64) -> Result<(), HostError> {
        let mut entries = self.entries.lock().unwrap();
        let before = entries.len();
        entries.retain(|entry| entry.id != id);
        if entries.len() == before {
            return Err(HostError::new(HostErrorCode::NotFound, "no such memory"));
        }
        Ok(())
    }
}

/// Answers every request with `action` and records what it was asked.
struct Answer {
    action: &'static str,
    asked: Mutex<Vec<ApprovalRequest>>,
}

#[async_trait]
impl ApprovalChannel for Answer {
    async fn request(&self, request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
        self.asked.lock().unwrap().push(request);
        Ok(ApprovalOutcome::Decided {
            action: self.action.to_owned(),
            value: json!({"reason": "the person said no"}),
        })
    }
}

async fn offered(tools: &MemoryTools) -> Vec<Arc<dyn Tool>> {
    tools
        .toolset()
        .tools(Arc::new(SimpleToolContext::new("memory-test")))
        .await
        .unwrap()
}

fn names(tools: &[Arc<dyn Tool>]) -> Vec<&str> {
    tools.iter().map(|tool| tool.name()).collect()
}

async fn call(tools: &[Arc<dyn Tool>], name: &str, args: Value) -> Value {
    let tool = tools.iter().find(|tool| tool.name() == name).unwrap();
    tool.execute(Arc::new(SimpleToolContext::new("memory-test")), args)
        .await
        .unwrap()
}

fn writable(store: &Arc<FakeStore>) -> MemoryTools {
    MemoryTools::new(store.clone()).with_writes(true)
}

#[tokio::test]
async fn without_writes_only_search_is_offered() {
    let store = Arc::new(FakeStore::default());
    let tools = offered(&MemoryTools::new(store.clone())).await;
    assert_eq!(names(&tools), [SEARCH_MEMORY]);
    let tools = offered(&writable(&store)).await;
    assert_eq!(names(&tools), TOOL_NAMES);
}

#[tokio::test]
async fn a_closed_gate_hides_the_writes_and_refuses_a_call_already_planned() {
    let store = Arc::new(FakeStore::default());
    let open = Arc::new(AtomicBool::new(false));
    let gate = {
        let open = open.clone();
        Arc::new(move || open.load(Ordering::SeqCst)) as WriteGate
    };
    let memory = writable(&store).with_write_gate(gate);
    assert_eq!(names(&offered(&memory).await), [SEARCH_MEMORY]);
    open.store(true, Ordering::SeqCst);
    let tools = offered(&memory).await;
    assert_eq!(names(&tools), TOOL_NAMES);
    open.store(false, Ordering::SeqCst);
    let answer = call(&tools, SAVE_MEMORY, json!({"content": "uses pnpm"})).await;
    assert_eq!(answer["code"], "memory_write_disabled");
    assert!(store.saves.lock().unwrap().is_empty());
}

#[tokio::test]
async fn the_schemas_are_the_issues() {
    let store = Arc::new(FakeStore::default());
    let tools = offered(&writable(&store)).await;
    let schema = |name: &str| {
        tools
            .iter()
            .find(|tool| tool.name() == name)
            .unwrap()
            .parameters_schema()
            .unwrap()
    };
    assert_eq!(
        schema(SAVE_MEMORY),
        json!({"type": "object", "properties": {"content": {"type": "string", "minLength": 1, "maxLength": 2000}},
            "required": ["content"], "additionalProperties": false})
    );
    assert_eq!(
        schema(SEARCH_MEMORY),
        json!({"type": "object", "properties": {"query": {"type": "string", "maxLength": 200},
            "limit": {"type": "integer", "minimum": 1, "maximum": 50, "default": 10}},
            "required": ["query"], "additionalProperties": false})
    );
    assert_eq!(
        schema(DELETE_MEMORY),
        json!({"type": "object", "properties": {"id": {"type": "integer", "minimum": 1}},
            "required": ["id"], "additionalProperties": false})
    );
    let save = tools
        .iter()
        .find(|tool| tool.name() == SAVE_MEMORY)
        .unwrap();
    assert!(save.description().contains("not on this device"));
}

#[tokio::test]
async fn search_is_read_only_and_concurrency_safe() {
    let store = Arc::new(FakeStore::default());
    for tool in offered(&writable(&store)).await {
        let search = tool.name() == SEARCH_MEMORY;
        assert_eq!(tool.is_read_only(), search, "{}", tool.name());
        assert_eq!(tool.is_concurrency_safe(), search, "{}", tool.name());
    }
}

#[tokio::test]
async fn save_refuses_content_that_looks_like_a_secret() {
    let store = Arc::new(FakeStore::default());
    let tools = offered(&writable(&store)).await;
    for secret in [
        "the deploy key is ghp_0123456789abcdefghijklmnopqrstuvwxyzAB",
        "AWS: AKIAIOSFODNN7EXAMPLE",
        "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA",
        "api_key = 9f8e7d6c5b4a",
        "use password: hunter2hunter2",
        "Authorization: Bearer abcdefghijklmnop0123",
        "the token is elnat_0123456789abcdefghijk",
        "db at postgres://app:s3cr3t@db.internal/app",
        "OPENAI sk-proj-0123456789abcdefghijkl",
    ] {
        let answer = call(&tools, SAVE_MEMORY, json!({"content": secret})).await;
        assert_eq!(answer["code"], "looks_like_secret", "{secret}");
    }
    assert!(store.saves.lock().unwrap().is_empty(), "nothing was saved");
    let long = "a".repeat(MAX_CONTENT_CHARS + 1);
    let answer = call(&tools, SAVE_MEMORY, json!({"content": long})).await;
    assert_eq!(answer["code"], "invalid_argument");
    for fact in [
        "This repo uses pnpm, never npm.",
        "The user prefers British spelling.",
        "Run tests with `cargo test -p elitea-local-tools`; the password reset flow lives in auth/.",
        "Docs are at https://docs.example.com/guide",
        "api_key: stored in the team 1Password vault",
        "password: rotated every quarter by the platform team",
    ] {
        assert!(!looks_like_secret(fact), "{fact}");
    }
    let answer = call(&tools, SAVE_MEMORY, json!({"content": " uses pnpm "})).await;
    assert_eq!(answer, json!({"status": "ok", "id": 1}));
    assert_eq!(*store.saves.lock().unwrap(), ["uses pnpm"]);
}

#[tokio::test]
async fn search_answers_data_bounded_by_its_limit() {
    let store = Arc::new(FakeStore::default());
    let tools = offered(&writable(&store)).await;
    for n in 0..3 {
        call(&tools, SAVE_MEMORY, json!({"content": format!("fact {n}")})).await;
    }
    let answer = call(&tools, SEARCH_MEMORY, json!({"query": "FACT", "limit": 2})).await;
    assert_eq!(
        answer,
        json!({"status": "ok", "memories": [{"id": 1, "content": "fact 0"}, {"id": 2, "content": "fact 1"}],
            "note": DATA_NOTE})
    );
    let answer = call(&tools, SEARCH_MEMORY, json!({"query": "x", "limit": 51})).await;
    assert_eq!(answer["code"], "invalid_argument");
}

#[tokio::test]
async fn delete_maps_not_found() {
    let store = Arc::new(FakeStore::default());
    let tools = offered(&writable(&store)).await;
    let answer = call(&tools, DELETE_MEMORY, json!({"id": 7})).await;
    assert_eq!(answer["status"], "error");
    assert_eq!(answer["code"], "not_found");
    call(&tools, SAVE_MEMORY, json!({"content": "fact"})).await;
    let answer = call(&tools, DELETE_MEMORY, json!({"id": 1})).await;
    assert_eq!(answer, json!({"status": "ok", "id": 1}));
    let answer = call(&tools, DELETE_MEMORY, json!({"id": 0})).await;
    assert_eq!(answer["code"], "invalid_argument");
}

#[tokio::test]
async fn every_call_is_asked_first_and_a_rejection_writes_nothing() {
    let store = Arc::new(FakeStore::default());
    let channel = Arc::new(Answer {
        action: "reject",
        asked: Mutex::new(Vec::new()),
    });
    let tools = offered(&writable(&store).with_approvals(channel.clone())).await;
    let answer = call(&tools, SAVE_MEMORY, json!({"content": "fact"})).await;
    assert_eq!(answer["code"], "rejected");
    assert!(
        answer["message"]
            .as_str()
            .unwrap()
            .contains("the person said no")
    );
    assert!(store.saves.lock().unwrap().is_empty());
    // A delete of an id the person has none of is answered without asking.
    let answer = call(&tools, DELETE_MEMORY, json!({"id": 3})).await;
    assert_eq!(answer["code"], "not_found");
    assert_eq!(channel.asked.lock().unwrap().len(), 1);
    store.entries.lock().unwrap().push(MemoryEntry {
        id: 3,
        content: "uses pnpm".to_owned(),
    });
    let answer = call(&tools, DELETE_MEMORY, json!({"id": 3})).await;
    assert_eq!(answer["code"], "rejected");
    assert_eq!(
        store.entries.lock().unwrap().len(),
        1,
        "nothing was deleted"
    );
    let asked = channel.asked.lock().unwrap();
    assert_eq!(
        asked[0].payload,
        json!({APPROVAL_PAYLOAD_KEY: {"tool": SAVE_MEMORY, "content": "fact"}})
    );
    // The approval shows what would be deleted.
    assert_eq!(
        asked[1].payload,
        json!({APPROVAL_PAYLOAD_KEY: {"tool": DELETE_MEMORY, "id": 3, "content": "uses pnpm"}})
    );
}

/// Defers every request, or fails it with `code`.
struct Unanswered(Option<HostErrorCode>);

#[async_trait]
impl ApprovalChannel for Unanswered {
    async fn request(&self, _request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
        match self.0 {
            None => Ok(ApprovalOutcome::Deferred),
            Some(code) => Err(HostError::new(code, "closed")),
        }
    }
}

#[tokio::test]
async fn a_deferred_approval_is_pending_and_a_closed_channel_is_not() {
    for (channel, code) in [
        (Unanswered(None), "approval_pending"),
        (
            Unanswered(Some(HostErrorCode::ExecutionEnded)),
            "execution_ended",
        ),
        (
            Unanswered(Some(HostErrorCode::DependencyUnavailable)),
            "approval_unavailable",
        ),
    ] {
        let store = Arc::new(FakeStore::default());
        let tools = offered(&writable(&store).with_approvals(Arc::new(channel))).await;
        let answer = call(&tools, SAVE_MEMORY, json!({"content": "fact"})).await;
        assert_eq!(answer["code"], code);
        assert!(
            store.saves.lock().unwrap().is_empty(),
            "{code}: nothing saved"
        );
    }
}
