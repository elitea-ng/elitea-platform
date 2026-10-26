//! Steering: messages the person sends while a local turn runs (IPC.md,
//! "Steering").
//!
//! A message sent mid-turn goes into the turn's queue
//! (`agent_turn_steer`). The person can change or remove it while it waits
//! (`agent_turn_steer_update`). At the next safe point of the run (after the
//! tool results of a step, before the next model request) the agent loop
//! takes the queue ([`SteeringInjector`]): each message becomes a user
//! message of the same turn. The host tells the UI (`steer_injected`) and
//! appends the message to the session, so the next turns' history has it.
//!
//! When the turn ends first (any end: answered, failed, cancelled, stopped
//! at a limit), each message that still waits is dropped with reason
//! `turn_ended`, before `done`. The UI then sends them as the next turn.
//!
//! The commit of the turn carries the messages the agent took, in order
//! (`steering_messages`, [`TurnSteering::commit_messages`]): the server
//! stores them after the question, as the session has them. The commit
//! carries at most [`MAX_COMMITTED`] of them, so one turn takes at most
//! that many: the taken and the waiting messages together. The queue
//! refuses the next one (`steer_turn_full`), and the UI sends it as the
//! next turn. No text is joined into another or cut.
//!
//! The queue and its events change under one lock, so the UI receives
//! `steer_queued`, `steer_injected` and `steer_dropped` of one message in
//! the order the queue saw them.

use std::sync::{Arc, Mutex, MutexGuard, PoisonError};

use adk_agent::SteeringSource;
use adk_core::{Content, Event, InvocationContext};
use adk_session::SessionService;
use async_trait::async_trait;
use serde_json::{Value, json};

use super::events::TurnEvents;
use super::turn::TurnError;

/// The most messages that can wait in one turn's queue.
pub const MAX_PENDING: usize = 10;
/// The largest message, in bytes of UTF-8.
pub const MAX_TEXT_BYTES: usize = 16 * 1024;
/// The most steering messages one commit carries (the server's bound), and
/// so the most that one turn takes (taken and waiting together).
pub const MAX_COMMITTED: usize = 10;
/// The author of a steering message's session event: the runner's own
/// author for the person's message.
const USER_AUTHOR: &str = "user";

/// The refusal for a turn that does not run (it ends, it was cancelled, or
/// the host does not know it as running).
pub fn not_running() -> TurnError {
    TurnError::new(
        "turn_not_running",
        "The agent is not working on this turn now. Send the message as a new turn.",
    )
}

fn not_pending() -> TurnError {
    TurnError::new(
        "steer_not_pending",
        "This message is not waiting any more: the agent has it already, or it was removed.",
    )
}

/// Check a message's text: not empty, not only whitespace (a NUL counts as
/// a space, as in the commit), not too long.
fn checked(text: &str) -> Result<String, TurnError> {
    if text.replace('\0', " ").trim().is_empty() {
        return Err(TurnError::new("steer_invalid", "The message is empty."));
    }
    if text.len() > MAX_TEXT_BYTES {
        return Err(TurnError::new(
            "steer_invalid",
            format!(
                "The message is too long: at most {} KiB.",
                MAX_TEXT_BYTES / 1024
            ),
        ));
    }
    Ok(text.to_owned())
}

/// The texts as the server accepts them: no NUL (a space instead). Each
/// text is one item, as the agent took it. [`checked`] keeps each one not
/// blank and at most [`MAX_TEXT_BYTES`], and [`TurnSteering::queue`] keeps
/// their number at most [`MAX_COMMITTED`].
fn committed(texts: &[String]) -> Vec<String> {
    debug_assert!(texts.len() <= MAX_COMMITTED);
    texts.iter().map(|text| text.replace('\0', " ")).collect()
}

fn turn_full() -> TurnError {
    TurnError::new(
        "steer_turn_full",
        format!(
            "This turn already has {MAX_COMMITTED} steering messages, the most one turn takes. Send this one as the next message."
        ),
    )
}

/// One message that waits for the next safe point.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Pending {
    pub id: String,
    pub text: String,
    /// 1 when queued; one more for each change.
    pub revision: u32,
}

#[derive(Default)]
struct Queue {
    /// False once the turn stops (cancel) or ends: nothing more is queued
    /// or injected.
    closed: bool,
    pending: Vec<Pending>,
    /// The texts the agent took, in the order it took them.
    injected: Vec<String>,
}

/// One turn's steering queue, with the turn's event stream.
pub struct TurnSteering {
    events: Arc<TurnEvents>,
    queue: Mutex<Queue>,
}

impl TurnSteering {
    #[must_use]
    pub fn new(events: Arc<TurnEvents>) -> Self {
        Self {
            events,
            queue: Mutex::new(Queue::default()),
        }
    }

    fn lock(&self) -> MutexGuard<'_, Queue> {
        self.queue.lock().unwrap_or_else(PoisonError::into_inner)
    }

    fn send_queued(&self, pending: &Pending) {
        self.events.send(
            "steer_queued",
            json!({ "steer_id": pending.id, "text": pending.text, "revision": pending.revision }),
        );
    }

    fn send_dropped(&self, pending: &Pending, reason: &str) {
        self.events.send(
            "steer_dropped",
            json!({ "steer_id": pending.id, "text": pending.text, "reason": reason }),
        );
    }

    /// Queue `text`; returns its id. Sends `steer_queued`.
    ///
    /// # Errors
    ///
    /// `turn_not_running` when the queue is closed, `steer_invalid` for an
    /// empty or too long text, `steer_queue_full` past [`MAX_PENDING`],
    /// `steer_turn_full` when the taken and the waiting messages are
    /// [`MAX_COMMITTED`] already (the commit cannot carry one more).
    pub fn queue(&self, text: &str) -> Result<String, TurnError> {
        let text = checked(text)?;
        let mut queue = self.lock();
        if queue.closed {
            return Err(not_running());
        }
        if queue.pending.len() >= MAX_PENDING {
            return Err(TurnError::new(
                "steer_queue_full",
                format!(
                    "At most {MAX_PENDING} messages can wait for the agent. Wait for it to take them, or remove one."
                ),
            ));
        }
        if queue.injected.len() + queue.pending.len() >= MAX_COMMITTED {
            return Err(turn_full());
        }
        let pending = Pending {
            id: uuid::Uuid::new_v4().to_string(),
            text,
            revision: 1,
        };
        self.send_queued(&pending);
        let id = pending.id.clone();
        queue.pending.push(pending);
        Ok(id)
    }

    /// Change a waiting message (`Some`), or remove it (`None`). Sends
    /// `steer_queued` with the next revision, or `steer_dropped` `removed`.
    ///
    /// # Errors
    ///
    /// `turn_not_running` when the queue is closed, `steer_not_pending`
    /// when no message with `steer_id` waits, `steer_invalid` for an empty
    /// or too long text.
    pub fn update(&self, steer_id: &str, text: Option<&str>) -> Result<(), TurnError> {
        let text = text.map(checked).transpose()?;
        let mut queue = self.lock();
        if queue.closed {
            return Err(not_running());
        }
        let index = queue
            .pending
            .iter()
            .position(|pending| pending.id == steer_id)
            .ok_or_else(not_pending)?;
        match text {
            Some(text) => {
                let pending = &mut queue.pending[index];
                pending.text = text;
                pending.revision += 1;
                let pending = pending.clone();
                self.send_queued(&pending);
            }
            None => {
                let pending = queue.pending.remove(index);
                self.send_dropped(&pending, "removed");
            }
        }
        Ok(())
    }

    /// The turn was cancelled: nothing more is queued or injected. The
    /// waiting messages stay for [`Self::end`].
    pub fn close(&self) {
        self.lock().closed = true;
    }

    /// The turn ended: close the queue, and drop each waiting message with
    /// reason `turn_ended` (the UI sends them as the next turn).
    pub fn end(&self) {
        let mut queue = self.lock();
        queue.closed = true;
        for pending in std::mem::take(&mut queue.pending) {
            self.send_dropped(&pending, "turn_ended");
        }
    }

    /// Take every waiting message, oldest first, and send `steer_injected`
    /// for each. Nothing when the queue is closed.
    pub fn take(&self) -> Vec<Pending> {
        let mut queue = self.lock();
        if queue.closed {
            return Vec::new();
        }
        let taken = std::mem::take(&mut queue.pending);
        for pending in &taken {
            self.events.send(
                "steer_injected",
                json!({ "steer_id": pending.id, "text": pending.text }),
            );
        }
        queue
            .injected
            .extend(taken.iter().map(|pending| pending.text.clone()));
        taken
    }

    /// The commit's `steering_messages`: the texts the agent took, in
    /// order, as `{content}`. Empty when it took none.
    #[must_use]
    pub fn commit_messages(&self) -> Vec<Value> {
        committed(&self.lock().injected)
            .into_iter()
            .map(|content| json!({ "content": content }))
            .collect()
    }

    /// The messages that wait now, oldest first.
    #[cfg(test)]
    #[must_use]
    pub fn pending(&self) -> Vec<Pending> {
        self.lock().pending.clone()
    }
}

/// The agent loop's [`SteeringSource`] for one turn: it takes the queue and
/// appends each message to the turn's session.
pub struct SteeringInjector {
    pub steering: Arc<TurnSteering>,
    pub service: Arc<dyn SessionService>,
    pub session_id: String,
}

impl SteeringInjector {
    /// The queued messages as user contents, each also appended to the
    /// session as a `user` event of `invocation_id`. A session that refuses
    /// the event is logged: the model still gets the message in this turn.
    pub async fn inject(&self, invocation_id: &str) -> Vec<Content> {
        let mut contents = Vec::new();
        for pending in self.steering.take() {
            let content = Content::new("user").with_text(pending.text);
            let mut event = Event::new(invocation_id);
            USER_AUTHOR.clone_into(&mut event.author);
            event.llm_response.content = Some(content.clone());
            if let Err(error) = self.service.append_event(&self.session_id, event).await {
                log::warn!("a steering message was not kept in the session: {error}");
            }
            contents.push(content);
        }
        if !contents.is_empty() {
            log::info!(
                "a local turn took {} steering message(s) at a safe point",
                contents.len()
            );
        }
        contents
    }
}

#[async_trait]
impl SteeringSource for SteeringInjector {
    async fn take(&self, ctx: Arc<dyn InvocationContext>) -> Vec<Content> {
        self.inject(ctx.invocation_id()).await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::d0::events::VecEmitter;
    use adk_core::{AdkError, Part};
    use adk_session::{CreateRequest, DeleteRequest, GetRequest, ListRequest, Session};

    fn steering() -> (Arc<VecEmitter>, TurnSteering) {
        let emitter = Arc::new(VecEmitter::default());
        let events = Arc::new(TurnEvents::new("turn-1".into(), emitter.clone()));
        (emitter, TurnSteering::new(events))
    }

    fn payloads(emitter: &VecEmitter) -> Vec<(&'static str, Value)> {
        emitter
            .all()
            .into_iter()
            .map(|event| (event.kind, event.payload))
            .collect()
    }

    #[test]
    fn steering_commits_the_taken_texts_in_order() {
        let (_, steering) = steering();
        assert!(steering.commit_messages().is_empty());
        steering.queue("first").unwrap();
        steering.take();
        let removed = steering.queue("removed").unwrap();
        steering.update(&removed, None).unwrap();
        steering.queue("second\0part").unwrap();
        steering.take();
        steering.queue("never taken").unwrap();
        steering.end();
        assert_eq!(
            steering.commit_messages(),
            vec![
                json!({ "content": "first" }),
                json!({ "content": "second part" })
            ]
        );
    }

    // One turn takes at most MAX_COMMITTED messages (taken and waiting
    // together): the next one is refused, and the commit carries each taken
    // text whole, one item each.
    #[test]
    fn steering_refuses_past_the_commit_bound_and_commits_each_text_whole() {
        let (_, steering) = steering();
        let long = "é".repeat(MAX_TEXT_BYTES / 2);
        for n in 1..MAX_COMMITTED {
            steering.queue(&format!("{n}{}", &long[2..])).unwrap();
            if n % 3 == 0 {
                steering.take();
            }
        }
        let last = steering.queue("last").unwrap();
        let refused = steering.queue("one more").unwrap_err();
        assert_eq!(refused.code, "steer_turn_full");
        assert!(refused.message.contains("next message"));
        // A removed waiting message frees its place.
        steering.update(&last, None).unwrap();
        steering.queue("last again").unwrap();
        steering.take();
        assert_eq!(steering.queue("after").unwrap_err().code, "steer_turn_full");
        let commit = steering.commit_messages();
        assert_eq!(commit.len(), MAX_COMMITTED);
        for (n, item) in commit.iter().take(MAX_COMMITTED - 1).enumerate() {
            let content = item["content"].as_str().unwrap();
            assert!(content.starts_with(&(n + 1).to_string()));
            assert!(content.len() <= MAX_TEXT_BYTES);
        }
        assert_eq!(
            commit[MAX_COMMITTED - 1],
            json!({ "content": "last again" })
        );
    }

    #[test]
    fn steering_queues_in_order_and_takes_all_once() {
        let (emitter, steering) = steering();
        let first = steering.queue("first").unwrap();
        let second = steering.queue("second").unwrap();
        assert_ne!(first, second);
        let taken = steering.take();
        assert_eq!(
            taken.iter().map(|p| p.text.as_str()).collect::<Vec<_>>(),
            ["first", "second"]
        );
        assert!(steering.take().is_empty());
        assert_eq!(
            emitter.kinds(),
            [
                "steer_queued",
                "steer_queued",
                "steer_injected",
                "steer_injected"
            ]
        );
        let events = payloads(&emitter);
        assert_eq!(events[0].1["revision"], 1);
        assert_eq!(events[2].1["steer_id"], first.as_str());
        assert_eq!(events[3].1["text"], "second");
    }

    #[test]
    fn steering_edit_bumps_the_revision_and_remove_drops() {
        let (emitter, steering) = steering();
        let id = steering.queue("draft").unwrap();
        steering.update(&id, Some("final")).unwrap();
        assert_eq!(steering.pending()[0].revision, 2);
        assert_eq!(steering.pending()[0].text, "final");
        steering.update(&id, None).unwrap();
        assert!(steering.pending().is_empty());
        let error = steering.update(&id, Some("again")).unwrap_err();
        assert_eq!(error.code, "steer_not_pending");
        let events = payloads(&emitter);
        assert_eq!(events[1].0, "steer_queued");
        assert_eq!(events[1].1["revision"], 2);
        assert_eq!(events[2].0, "steer_dropped");
        assert_eq!(events[2].1["reason"], "removed");
        assert_eq!(events[2].1["text"], "final");
    }

    #[test]
    fn steering_refuses_invalid_text_and_a_full_queue() {
        let (_, steering) = steering();
        assert_eq!(steering.queue("  \n\t").unwrap_err().code, "steer_invalid");
        // Only NUL is blank in the commit (a NUL becomes a space).
        assert_eq!(steering.queue("\0 \0").unwrap_err().code, "steer_invalid");
        let long = "x".repeat(MAX_TEXT_BYTES + 1);
        assert_eq!(steering.queue(&long).unwrap_err().code, "steer_invalid");
        assert!(steering.queue(&"x".repeat(MAX_TEXT_BYTES)).is_ok());
        for n in 1..MAX_PENDING {
            steering.queue(&format!("m{n}")).unwrap();
        }
        assert_eq!(
            steering.queue("one more").unwrap_err().code,
            "steer_queue_full"
        );
        let id = steering.pending()[0].id.clone();
        assert_eq!(
            steering.update(&id, Some(" ")).unwrap_err().code,
            "steer_invalid"
        );
    }

    #[test]
    fn steering_end_drops_the_waiting_messages_and_closes() {
        let (emitter, steering) = steering();
        steering.queue("a").unwrap();
        steering.queue("b").unwrap();
        steering.close();
        // A cancelled turn injects nothing more, and queues nothing more.
        assert!(steering.take().is_empty());
        assert_eq!(steering.queue("c").unwrap_err().code, "turn_not_running");
        let id = steering.pending()[0].id.clone();
        assert_eq!(
            steering.update(&id, None).unwrap_err().code,
            "turn_not_running"
        );
        steering.end();
        assert!(steering.pending().is_empty());
        let dropped: Vec<_> = payloads(&emitter)
            .into_iter()
            .filter(|(kind, _)| *kind == "steer_dropped")
            .map(|(_, payload)| (payload["text"].clone(), payload["reason"].clone()))
            .collect();
        assert_eq!(
            dropped,
            [
                (json!("a"), json!("turn_ended")),
                (json!("b"), json!("turn_ended"))
            ]
        );
        // A second end sends nothing more.
        steering.end();
        assert_eq!(emitter.kinds().len(), 4);
    }

    /// Records the appended events; nothing else is used.
    #[derive(Default)]
    struct Appended(Mutex<Vec<(String, Event)>>);

    #[async_trait]
    impl SessionService for Appended {
        async fn create(&self, _: CreateRequest) -> adk_core::Result<Box<dyn Session>> {
            Err(AdkError::session("not used"))
        }
        async fn get(&self, _: GetRequest) -> adk_core::Result<Box<dyn Session>> {
            Err(AdkError::session("not used"))
        }
        async fn list(&self, _: ListRequest) -> adk_core::Result<Vec<Box<dyn Session>>> {
            Ok(Vec::new())
        }
        async fn delete(&self, _: DeleteRequest) -> adk_core::Result<()> {
            Ok(())
        }
        async fn append_event(&self, session_id: &str, event: Event) -> adk_core::Result<()> {
            self.0.lock().unwrap().push((session_id.to_owned(), event));
            Ok(())
        }
    }

    #[tokio::test]
    async fn steering_injector_returns_user_contents_and_appends_user_events() {
        let (emitter, steering) = steering();
        let steering = Arc::new(steering);
        steering.queue("look at src/").unwrap();
        steering.queue("and then tests").unwrap();
        let service = Arc::new(Appended::default());
        let injector = SteeringInjector {
            steering: steering.clone(),
            service: service.clone(),
            session_id: "session-1".into(),
        };
        let contents = injector.inject("invocation-1").await;
        let texts: Vec<_> = contents
            .iter()
            .map(|content| {
                assert_eq!(content.role, "user");
                match &content.parts[..] {
                    [Part::Text { text }] => text.clone(),
                    other => panic!("unexpected parts {other:?}"),
                }
            })
            .collect();
        assert_eq!(texts, ["look at src/", "and then tests"]);
        assert!(injector.inject("invocation-1").await.is_empty());
        let appended = service.0.lock().unwrap();
        assert_eq!(appended.len(), 2);
        for ((session_id, event), text) in appended.iter().zip(&texts) {
            assert_eq!(session_id, "session-1");
            assert_eq!(event.author, "user");
            assert_eq!(event.invocation_id, "invocation-1");
            assert_eq!(
                event.llm_response.content.as_ref().unwrap().parts,
                vec![Part::Text { text: text.clone() }]
            );
        }
        assert_eq!(
            emitter.kinds(),
            [
                "steer_queued",
                "steer_queued",
                "steer_injected",
                "steer_injected"
            ]
        );
    }
}
