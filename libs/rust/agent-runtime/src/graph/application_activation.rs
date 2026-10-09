//! Separate visits to one fixed ADK subgraph thread without deleting receipts.

use std::sync::{Arc, Mutex};

use adk_graph::checkpoint::RetentionPolicy;
use adk_graph::{Checkpoint, Checkpointer, GraphError};
use async_trait::async_trait;
use ring::digest;
use serde::{Deserialize, Serialize};
use serde_json::Value;

const ACTIVATION_METADATA_KEY: &str = "elitea.pipeline.application-activation.v1";
const ACTIVATION_SCHEMA: &str = "elitea.pipeline.application-activation.v1";

#[derive(Clone, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
struct ApplicationActivation {
    schema: String,
    parent_thread: String,
    node_name: String,
    parent_step: usize,
    definition_digest: [u8; 32],
    input_digest: [u8; 32],
}

impl ApplicationActivation {
    fn child_thread(&self) -> String {
        format!("{}/{}", self.parent_thread, self.node_name)
    }
}

/// Every node owns its own overlay. The inner adapter retains authority and fencing.
pub struct ApplicationActivationCheckpointer {
    inner: Arc<dyn Checkpointer>,
    active: Arc<Mutex<Option<ApplicationActivation>>>,
}

impl ApplicationActivationCheckpointer {
    pub fn new(inner: Arc<dyn Checkpointer>) -> Self {
        Self {
            inner,
            active: Arc::new(Mutex::new(None)),
        }
    }

    pub fn enter(
        &self,
        parent_thread: &str,
        node_name: &str,
        parent_step: usize,
        definition_digest: [u8; 32],
        inputs: &Value,
    ) -> Result<ApplicationActivationGuard, GraphError> {
        if parent_thread.is_empty()
            || parent_thread.len() > 480
            || parent_thread.chars().any(char::is_control)
            || definition_digest == [0; 32]
            || !super::yaml::valid_graph_id(node_name)
        {
            return Err(activation_error());
        }
        let input_digest = input_digest(inputs)?;
        let activation = ApplicationActivation {
            schema: ACTIVATION_SCHEMA.to_owned(),
            parent_thread: parent_thread.to_owned(),
            node_name: node_name.to_owned(),
            parent_step,
            definition_digest,
            input_digest,
        };
        let mut stored = self.active.lock().map_err(|_| activation_error())?;
        if stored.is_some() {
            return Err(activation_error());
        }
        *stored = Some(activation);
        Ok(ApplicationActivationGuard {
            active: Arc::clone(&self.active),
        })
    }

    fn activation(&self) -> Result<Option<ApplicationActivation>, GraphError> {
        self.active
            .lock()
            .map(|value| value.clone())
            .map_err(|_| activation_error())
    }

    fn for_activation(
        checkpoint: Checkpoint,
        activation: Option<&ApplicationActivation>,
    ) -> Result<Option<Checkpoint>, GraphError> {
        let Some(activation) = activation else {
            return Ok(Some(checkpoint));
        };
        let thread = activation.child_thread();
        if checkpoint.thread_id != thread
            && !checkpoint.thread_id.starts_with(&format!("{thread}/"))
        {
            return Ok(Some(checkpoint));
        }
        let family = checkpoint
            .metadata
            .get(ACTIVATION_METADATA_KEY)
            .and_then(Value::as_object)
            .ok_or_else(activation_error)?;
        let recorded: ApplicationActivation =
            serde_json::from_value(family.get(&thread).ok_or_else(activation_error)?.clone())
                .map_err(|_| activation_error())?;
        if recorded.schema != ACTIVATION_SCHEMA
            || recorded.child_thread() != thread
            || recorded.definition_digest != activation.definition_digest
            || (recorded.parent_step == activation.parent_step
                && recorded.input_digest != activation.input_digest)
        {
            return Err(activation_error());
        }
        if recorded == *activation {
            return Ok(Some(checkpoint));
        }
        // A completed previous visit starts a new activation. An unfinished or
        // unproven visit cannot be treated as a fresh run and repeat effects.
        if checkpoint.pending_nodes.is_empty() && recorded.parent_step < activation.parent_step {
            Ok(None)
        } else {
            Err(activation_error())
        }
    }
}

/// SHA-256 of the node's inputs in their canonical (sorted-member) encoding,
/// so the digest is the same whatever `serde_json`'s features are.
fn input_digest(inputs: &Value) -> Result<[u8; 32], GraphError> {
    let input = crate::canonical::to_vec(inputs).map_err(|_| activation_error())?;
    let mut input_digest = [0; 32];
    input_digest.copy_from_slice(digest::digest(&digest::SHA256, &input).as_ref());
    Ok(input_digest)
}

pub struct ApplicationActivationGuard {
    active: Arc<Mutex<Option<ApplicationActivation>>>,
}

impl Drop for ApplicationActivationGuard {
    fn drop(&mut self) {
        if let Ok(mut stored) = self.active.lock() {
            *stored = None;
        }
    }
}

#[async_trait]
impl Checkpointer for ApplicationActivationCheckpointer {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        let Some(activation) = self.activation()? else {
            return self.inner.save(checkpoint).await;
        };
        let thread = activation.child_thread();
        if checkpoint.thread_id != thread
            && !checkpoint.thread_id.starts_with(&format!("{thread}/"))
        {
            return self.inner.save(checkpoint).await;
        }
        let mut checkpoint = checkpoint.clone();
        let mut family = match checkpoint.metadata.remove(ACTIVATION_METADATA_KEY) {
            Some(Value::Object(family)) => family,
            None => serde_json::Map::new(),
            Some(_) => return Err(activation_error()),
        };
        if family.len() > 3 {
            return Err(activation_error());
        }
        family.insert(
            thread,
            serde_json::to_value(&activation).map_err(|_| activation_error())?,
        );
        if family.len() > 3 {
            return Err(activation_error());
        }
        checkpoint
            .metadata
            .insert(ACTIVATION_METADATA_KEY.to_owned(), Value::Object(family));
        self.inner.save(&checkpoint).await
    }

    async fn load(&self, thread_id: &str) -> Result<Option<Checkpoint>, GraphError> {
        let activation = self.activation()?;
        match self.inner.load(thread_id).await? {
            Some(checkpoint) => Self::for_activation(checkpoint, activation.as_ref()),
            None => Ok(None),
        }
    }

    async fn load_by_id(&self, checkpoint_id: &str) -> Result<Option<Checkpoint>, GraphError> {
        let activation = self.activation()?;
        match self.inner.load_by_id(checkpoint_id).await? {
            Some(checkpoint) => Self::for_activation(checkpoint, activation.as_ref()),
            None => Ok(None),
        }
    }

    async fn list(&self, thread_id: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.inner.list(thread_id).await
    }

    async fn delete(&self, thread_id: &str) -> Result<(), GraphError> {
        self.inner.delete(thread_id).await
    }

    async fn prune(&self, thread_id: &str, policy: &RetentionPolicy) -> Result<usize, GraphError> {
        self.inner.prune(thread_id, policy).await
    }
}

fn activation_error() -> GraphError {
    GraphError::NodeExecutionFailed {
        node: "application_activation".to_owned(),
        message: "the saved pipeline checkpoint belongs to another or unproven activation"
            .to_owned(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use adk_graph::{MemoryCheckpointer, State};
    use serde_json::json;

    /// The input digest binds a child activation to its inputs; it must not
    /// depend on `serde_json`'s `preserve_order` (ADR-0029 decision 2). The pin
    /// is the pre-move worker value.
    #[test]
    fn input_digest_is_independent_of_map_order() {
        let unordered: Value =
            serde_json::from_str(r#"{"task":"one","context":{"z":1,"a":[{"y":2,"x":3}]}}"#)
                .expect("fixture");
        let reordered: Value =
            serde_json::from_str(r#"{"context":{"a":[{"x":3,"y":2}],"z":1},"task":"one"}"#)
                .expect("fixture");
        let digest = input_digest(&unordered).expect("digest");
        assert_eq!(digest, input_digest(&reordered).expect("digest"));
        let hex = digest.iter().fold(String::new(), |mut hex, byte| {
            use std::fmt::Write as _;
            write!(hex, "{byte:02x}").expect("write to a String");
            hex
        });
        assert_eq!(
            hex,
            "a016bd2a1351e57ffa58603da9809e476392dbd1a3300828b141f6ebe724d5c5"
        );
    }

    #[tokio::test]
    async fn completed_old_visits_are_hidden_and_current_receipts_are_reused() {
        let inner: Arc<dyn Checkpointer> = Arc::new(MemoryCheckpointer::new());
        let overlay = ApplicationActivationCheckpointer::new(inner.clone());
        let first = overlay
            .enter("root", "delegate", 1, [1; 32], &json!({"task":"one"}))
            .unwrap();
        let checkpoint = Checkpoint::new("root/delegate", State::new(), 2, vec![]);
        let id = overlay.save(&checkpoint).await.unwrap();
        drop(first);
        let second = overlay
            .enter("root", "delegate", 3, [1; 32], &json!({"task":"two"}))
            .unwrap();
        assert!(overlay.load("root/delegate").await.unwrap().is_none());
        assert!(inner.load_by_id(&id).await.unwrap().is_some());
        let checkpoint = Checkpoint::new("root/delegate", State::new(), 2, vec![]);
        let next_id = overlay.save(&checkpoint).await.unwrap();
        drop(second);
        let _resume = overlay
            .enter("root", "delegate", 3, [1; 32], &json!({"task":"two"}))
            .unwrap();
        assert_eq!(
            overlay
                .load("root/delegate")
                .await
                .unwrap()
                .unwrap()
                .checkpoint_id,
            next_id
        );
    }

    #[tokio::test]
    async fn unfinished_foreign_and_unproven_visits_fail_without_resetting_the_inner_store() {
        let inner: Arc<dyn Checkpointer> = Arc::new(MemoryCheckpointer::new());
        let overlay = ApplicationActivationCheckpointer::new(inner.clone());
        let first = overlay
            .enter("root", "delegate", 1, [1; 32], &json!({"task":"one"}))
            .unwrap();
        let checkpoint =
            Checkpoint::new("root/delegate", State::new(), 2, vec!["effect".to_owned()]);
        let id = overlay.save(&checkpoint).await.unwrap();
        drop(first);
        let _second = overlay
            .enter("root", "delegate", 3, [1; 32], &json!({"task":"two"}))
            .unwrap();
        assert!(overlay.load("root/delegate").await.is_err());
        assert_eq!(
            inner.load_by_id(&id).await.unwrap().unwrap().checkpoint_id,
            id
        );
        inner
            .save(&Checkpoint::new("root/delegate", State::new(), 2, vec![]))
            .await
            .unwrap();
        assert!(overlay.load("root/delegate").await.is_err());
    }
    #[tokio::test]
    async fn a_completed_future_activation_cannot_restart_an_older_parent_frontier() {
        let inner: Arc<dyn Checkpointer> = Arc::new(MemoryCheckpointer::new());
        let overlay = ApplicationActivationCheckpointer::new(inner.clone());
        let future = overlay
            .enter("root", "delegate", 9, [1; 32], &json!({"task":"nine"}))
            .unwrap();
        let checkpoint = Checkpoint::new("root/delegate", State::new(), 2, vec![]);
        let id = overlay.save(&checkpoint).await.unwrap();
        drop(future);
        let _stale = overlay
            .enter("root", "delegate", 3, [1; 32], &json!({"task":"three"}))
            .unwrap();
        assert!(overlay.load("root/delegate").await.is_err());
        assert_eq!(
            inner.load_by_id(&id).await.unwrap().unwrap().checkpoint_id,
            id
        );
    }
}
