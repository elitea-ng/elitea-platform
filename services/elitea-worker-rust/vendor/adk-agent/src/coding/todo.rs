//! The `write_todos` planning tool.

use std::sync::{Arc, Mutex};

use adk_core::{Result, Tool, ToolContext};
use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

/// A single task in the agent's plan.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct TodoItem {
    /// What the step does.
    pub content: String,
    /// One of `pending`, `in_progress`, `completed`.
    #[serde(default = "default_status")]
    pub status: String,
}

fn default_status() -> String {
    "pending".to_string()
}

/// A planning tool: the model calls `write_todos` to record and update a short
/// task list. The list is held in shared state so the harness can surface it.
///
/// Cloning a `TodoTool` shares the same underlying list.
#[derive(Clone, Default)]
pub struct TodoTool {
    items: Arc<Mutex<Vec<TodoItem>>>,
}

impl TodoTool {
    /// Create a new, empty todo tool.
    pub fn new() -> Self {
        Self { items: Arc::new(Mutex::new(Vec::new())) }
    }

    /// A snapshot of the current list.
    pub fn items(&self) -> Vec<TodoItem> {
        self.items.lock().map(|g| g.clone()).unwrap_or_default()
    }
}

#[async_trait]
impl Tool for TodoTool {
    fn name(&self) -> &str {
        "write_todos"
    }

    fn description(&self) -> &str {
        "Record or update your task plan. Pass the full list of todos each time; \
         it replaces the previous list. Each todo has `content` and a `status` of \
         'pending', 'in_progress', or 'completed'. Keep exactly one item \
         'in_progress' while you work."
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "todos": {
                    "type": "array",
                    "description": "The full, updated task list.",
                    "items": {
                        "type": "object",
                        "properties": {
                            "content": { "type": "string", "description": "What this step does." },
                            "status": {
                                "type": "string",
                                "enum": ["pending", "in_progress", "completed"],
                                "description": "Current status."
                            }
                        },
                        "required": ["content", "status"]
                    }
                }
            },
            "required": ["todos"]
        }))
    }

    async fn execute(&self, _ctx: Arc<dyn ToolContext>, args: Value) -> Result<Value> {
        let todos = args
            .get("todos")
            .cloned()
            .ok_or_else(|| adk_core::AdkError::tool("missing required argument 'todos'"))?;
        let parsed: Vec<TodoItem> = serde_json::from_value(todos)
            .map_err(|e| adk_core::AdkError::tool(format!("invalid todos: {e}")))?;

        if let Ok(mut guard) = self.items.lock() {
            *guard = parsed.clone();
        }

        let remaining = parsed.iter().filter(|t| t.status != "completed").count();
        Ok(json!({
            "todos": parsed,
            "total": parsed.len(),
            "remaining": remaining,
            "message": format!("Plan updated: {} item(s), {remaining} remaining.", parsed.len()),
        }))
    }
}
