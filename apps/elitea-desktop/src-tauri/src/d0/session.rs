//! A turn's session: reopened from `sessions.sqlite` (or, when the store is
//! unavailable, an in-memory one seeded from the server), seeded from the
//! server conversation when it is missing, empty, marked for re-seeding or
//! behind the conversation, and settled when the turn ends.
//!
//! The call sites are in `turn.rs`; the storage is `crate::sessions` behind
//! the runtime's `StateStore`, so extraction stage 5 adopts it unchanged.

use std::sync::Arc;

use adk_session::{CreateRequest, InMemorySessionService, SessionService};
use elitea_agent_runtime::host::StateStore as _;
use elitea_local_tools::policy::LocalWorkPolicy;

use super::api::{AnsweringParticipant, ApiError, PlatformApi};
use crate::history::Owner;
use crate::seed::{self, SEED_MESSAGE_GROUPS};
use crate::sessions::{
    ADK_USER, APP_NAME, DesktopStateStore, NewEvent, SessionIdentity, SessionStore, TurnOutcome,
    TurnScope, policy_fingerprint,
};

/// The author the runner gives the agent's own events (`turn.rs` names the
/// agent with it): seeded events carry it so the runner's per-agent history
/// filter keeps them.
pub const AGENT_AUTHOR: &str = "elitea_agent";

/// What one turn runs its agent on.
pub struct SessionPlan {
    pub service: Arc<dyn SessionService>,
    pub session_id: String,
    /// The durable store behind it; `None` for the in-memory fallback.
    durable: Option<Durable>,
    /// The conversation's group count after this turn's two messages.
    cursor_after: Option<String>,
}

struct Durable {
    store: Arc<SessionStore>,
    key: String,
    scope: Option<TurnScope>,
}

/// Why a turn cannot take its session.
#[derive(Debug)]
pub enum SessionError {
    /// The conversation's transcript could not be read.
    Api(ApiError),
    /// Another turn of this conversation is running.
    Busy,
}

impl SessionPlan {
    /// Open (and seed, when needed) the session of the conversation.
    ///
    /// `owner` is who is signed in (`None` when the lookup failed: the turn
    /// then runs on an in-memory session). A store that fails here is
    /// logged and replaced by one too; the server rebuilds either.
    ///
    /// # Errors
    ///
    /// The transcript could not be read, or another turn holds the session.
    pub async fn open(
        store: Option<&Arc<DesktopStateStore>>,
        owner: Option<&Owner>,
        api: &PlatformApi,
        project_id: i64,
        policy: &LocalWorkPolicy,
        answering: &AnsweringParticipant,
        turn_tag: &str,
    ) -> Result<Self, SessionError> {
        let cursor_now = answering.message_count.map(|count| count.to_string());
        let cursor_after = answering.message_count.map(|count| (count + 2).to_string());
        let durable = match (store, owner) {
            (Some(store), Some(owner)) => {
                let identity = SessionIdentity {
                    deployment: owner.origin.clone(),
                    user_id: owner.user_id,
                    project_id,
                    conversation_uuid: answering.conversation_uuid.clone(),
                };
                let key = identity.key();
                match store
                    .store()
                    .open_session(&identity, &policy_fingerprint(&policy.path_deny))
                    .await
                {
                    Ok(state) => Some((store.clone(), key, state)),
                    Err(error) => {
                        log::warn!(
                            "the session store is unavailable, so this turn is seeded from the server: {error}"
                        );
                        None
                    }
                }
            }
            (Some(_), None) => {
                log::warn!("no session store for this turn: the signed-in user is unknown");
                None
            }
            (None, _) => None,
        };
        // The per-conversation claim precedes the destructive re-seed: a
        // second workspace turning this conversation is refused here, before
        // it can wipe this session's events (the claim lives until the turn
        // settles).
        let scope = match &durable {
            Some((state_store, key, _)) => Some(
                state_store
                    .store()
                    .begin_turn(key, turn_tag)
                    .map_err(|_| SessionError::Busy)?,
            ),
            None => None,
        };
        // Is the session what the server has? If not it is rebuilt from the
        // server's transcript (a conversation with no groups has none to
        // read, so a new conversation costs no request).
        let empty_conversation = answering.message_count == Some(0);
        let (needs_seed, strip) = match &durable {
            Some((_, _, state)) => (
                !(state.seeded_from_server && state.server_cursor == cursor_now),
                state.bodies_dropped,
            ),
            None => (!empty_conversation, false),
        };
        let events = if needs_seed && !empty_conversation {
            let (groups, steps) = api
                .server_transcript(
                    project_id,
                    &answering.conversation_uuid,
                    answering.numeric_id,
                    SEED_MESSAGE_GROUPS,
                )
                .await
                .map_err(SessionError::Api)?;
            seed::fold(&groups, &steps, AGENT_AUTHOR, strip)
        } else {
            Vec::new()
        };
        match durable {
            Some((state_store, key, _)) => {
                let store = state_store.store().clone();
                if needs_seed {
                    // Empty conversation or not, the session now agrees.
                    if let Err(error) = store.seed(&key, events, cursor_now).await {
                        log::warn!("the session could not be seeded: {error}");
                    }
                }
                Ok(Self {
                    service: state_store.sessions(),
                    session_id: key.clone(),
                    durable: Some(Durable { store, key, scope }),
                    cursor_after,
                })
            }
            None => {
                let service: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
                let session_id = uuid::Uuid::new_v4().to_string();
                let created = service
                    .create(CreateRequest {
                        app_name: APP_NAME.to_owned(),
                        user_id: ADK_USER.to_owned(),
                        session_id: Some(session_id.clone()),
                        state: std::collections::HashMap::new(),
                    })
                    .await;
                if created.is_err() {
                    log::warn!("the in-memory session could not be created");
                }
                for NewEvent { event, .. } in events {
                    if let Err(error) = service.append_event(&session_id, event).await {
                        log::warn!("a seeded event could not be kept: {error}");
                    }
                }
                Ok(Self {
                    service,
                    session_id,
                    durable: None,
                    cursor_after: None,
                })
            }
        }
    }

    /// The server created the turn: its events carry the execution's id.
    pub fn retag(&mut self, execution_id: &str) {
        if let Some(scope) = self
            .durable
            .as_mut()
            .and_then(|durable| durable.scope.as_mut())
        {
            scope.retag(execution_id);
        }
    }

    /// Settle the turn's events in the session (see [`TurnOutcome`]) and
    /// release it. `committed` carries the answer's message id.
    pub async fn settle(&mut self, execution_id: &str, committed: Result<&str, Refusal>) {
        let Some(durable) = self.durable.as_mut() else {
            return;
        };
        let outcome = match committed {
            Ok(response_message_id) => TurnOutcome::Committed {
                response_message_id: response_message_id.to_owned(),
                cursor: self.cursor_after.clone(),
            },
            Err(Refusal::Cancelled) => TurnOutcome::Cancelled,
            Err(Refusal::NotSaved) => TurnOutcome::Refused,
        };
        if let Err(error) = durable
            .store
            .finish_turn(&durable.key, execution_id, outcome)
            .await
        {
            log::warn!("the turn could not be settled in its session: {error}");
        }
        durable.scope = None;
    }
}

/// Why a turn's events are not the server's.
#[derive(Clone, Copy, Debug)]
pub enum Refusal {
    Cancelled,
    NotSaved,
}
