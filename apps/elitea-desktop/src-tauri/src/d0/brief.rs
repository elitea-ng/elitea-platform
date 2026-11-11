//! A `spawn_agent` brief on a workspace turn (#1827, ADR-0050 decision 9d):
//! what the desktop agent itself sees, and the check of a brief against it.
//!
//! * **Message ids.** Each message the agent may quote reaches the model
//!   after one line, `[message m-…]` (#1831's
//!   [`elitea_agent_runtime::subagents::message_ref`]): the person's
//!   messages and other participants' messages of a session seeded from the
//!   server (`crate::seed::fold`, by the message group's uuid), and this
//!   turn's message ([`TurnQuestion`], by the turn's `question_id`, which the
//!   platform keeps as the question group's uuid). The id is the cloud's for
//!   the same message, from the message's id alone, so the bytes are the
//!   same on every turn (the session stores them) and a re-seed gives the
//!   same line. The text after the line is neutralized, so no message can
//!   show another message's id; the person's steering messages are too
//!   (`super::steering`). The agent's own answers carry no id. A spawn's
//!   record (`brief_refs.message_ids`) names each quoted message by its
//!   chat message uuid, as a cloud spawn's does (#1908).
//! * **Excerpts** are checked against the model's last request (the turn's
//!   [`super::model::RequestCapture`]): the quote must be part of the text
//!   after a message's id line, as the model read it (each run of
//!   whitespace read as one space). A message the request no longer held
//!   (compacted away) cannot be quoted.
//! * **Attachments** are this turn's images: each is shown to the model as
//!   `[Attachment att-…: "name"]` after the message (an id over the image's
//!   bytes). A child is told the image's name only: it cannot view it.
//! * **Memory entries** are refused: the desktop has no `recall` tool (its
//!   memory tools search the person's memories), so no entry was recalled.
//! * **Facts** pass.

use std::collections::HashMap;
use std::fmt::Write as _;
use std::sync::{Arc, Mutex, OnceLock};

use adk_core::{Content, Part};
// The runtime's, so every host names a message and parses its id line one
// way (#1909).
use elitea_agent_runtime::subagents::message_ref::{neutralize, normalize_whitespace, tag};
pub use elitea_agent_runtime::subagents::message_ref::{short_message_ref, strip_tag};
use elitea_agent_runtime::subagents::profile::{
    BRIEF_ATTACHMENT_NOT_READABLE, BRIEF_EXCERPT_NOT_VISIBLE, BRIEF_MEMORY_NOT_RECALLED,
};
use elitea_agent_runtime::subagents::{Brief, CheckedBrief};
use sha2::{Digest as _, Sha256};

use super::attachments::Attachment;

/// The domain of [`attachment_id`]'s hash.
const ATTACHMENT_ID_DOMAIN: &[u8] = b"elitea.desktop.attachment-id.v1\0";

/// At most this many short ids [`MessageIds`] remembers; past it, it starts
/// over (a brief then keeps an older message's short id, as before #1908).
const MAX_KNOWN_MESSAGES: usize = 16 * 1024;

/// The chat message each short id this process showed a model stands for
/// (#1908): filled by [`tagged`] (a seeded message by its group's uuid, this
/// turn's message by its `question_id`), read by [`BriefView::check`], so a
/// spawn's record names the chat message, as a cloud spawn's does. A short
/// id is a hash of its message id, so an entry is never wrong; a message
/// shown before the app started (a session reopened from its store) has
/// none, and its record keeps the short id.
struct MessageIds;

impl MessageIds {
    fn known() -> &'static Mutex<HashMap<String, String>> {
        static KNOWN: OnceLock<Mutex<HashMap<String, String>>> = OnceLock::new();
        KNOWN.get_or_init(|| Mutex::new(HashMap::new()))
    }

    fn remember(short: &str, message_id: &str) {
        let Ok(mut known) = Self::known().lock() else {
            return;
        };
        if known.len() >= MAX_KNOWN_MESSAGES && !known.contains_key(short) {
            known.clear();
        }
        known.insert(short.to_owned(), canonical_message_id(message_id));
    }

    fn message_of(short: &str) -> Option<String> {
        Self::known().lock().ok()?.get(short).cloned()
    }
}

/// The chat message id as the record keeps it: the canonical uuid form (the
/// one the short id is over), else the id as it is.
fn canonical_message_id(message_id: &str) -> String {
    uuid::Uuid::try_parse(message_id)
        .map_or_else(|_| message_id.to_owned(), |id| id.hyphenated().to_string())
}

/// `text` as the model reads a message it may quote: the id line of
/// `message_id`, then `text` neutralized. Without an id, `text` neutralized
/// and no line (it cannot be named).
#[must_use]
pub fn tagged(message_id: &str, text: &str) -> String {
    let neutral = neutralize(text);
    if message_id.is_empty() {
        return neutral.into_owned();
    }
    let short = short_message_ref(message_id);
    MessageIds::remember(&short, message_id);
    let mut out = tag(&short);
    out.push_str(&neutral);
    out
}

/// The id of an attached image: `att-` and 8 hex digits of a SHA-256 over
/// its bytes, so the same image has the same id wherever it is shown.
#[must_use]
pub fn attachment_id(bytes: &[u8]) -> String {
    let mut hash = Sha256::new();
    hash.update(ATTACHMENT_ID_DOMAIN);
    hash.update(bytes);
    let digest = hash.finalize();
    let mut id = String::from("att-");
    for byte in &digest[..4] {
        let _ = write!(id, "{byte:02x}");
    }
    id
}

/// One quotable message of a request: its short id and its text after the
/// id line, whitespace-normalized. Only a user-role content whose first
/// text part opens with an id line is one; every other text that reaches
/// the model as a message is neutralized, so it cannot open with one.
fn quotable(content: &Content) -> Option<(String, String)> {
    if content.role != "user" {
        return None;
    }
    let first = content.parts.iter().find_map(|part| match part {
        Part::Text { text } => Some(text.as_str()),
        _ => None,
    })?;
    let (short, text) = strip_tag(first)?;
    Some((short.to_owned(), normalize_whitespace(text)))
}

/// `text` without the `[message m-…]` line it may open with (a message as
/// the person wrote it).
#[must_use]
pub fn without_tag(text: &str) -> &str {
    strip_tag(text).map_or(text, |(_, rest)| rest)
}

/// An image's name as a model or a child reads it: quoted-attribute safe
/// and neutralized, so it cannot close its quotes, add a line or show a
/// message's id.
fn shown_name(name: &str) -> String {
    neutralize(&super::framing::attribute_value(name)).into_owned()
}

/// The person's message of this turn as the model reads it: its id line,
/// the text neutralized, and a line per attached image.
pub struct TurnQuestion {
    text: String,
    attachments: Vec<(String, String)>,
}

impl TurnQuestion {
    /// `message` (the person's message, or a command's expansion) under the
    /// id of `question_id`, with `images`.
    #[must_use]
    pub fn new(question_id: &str, message: &str, images: &[Arc<Attachment>]) -> Self {
        let mut attachments: Vec<(String, String)> = Vec::new();
        for image in images {
            // One line per image: two names for the same bytes are both
            // shown (and either names the id).
            let entry = (attachment_id(&image.picture.bytes), image.name.clone());
            if !attachments.contains(&entry) {
                attachments.push(entry);
            }
        }
        Self {
            text: tagged(question_id, message),
            attachments,
        }
    }

    /// The note that names the images, its own text part after the
    /// message's (not part of what an excerpt may quote); `None` without.
    fn attachment_note(&self) -> Option<String> {
        (!self.attachments.is_empty()).then(|| {
            self.attachments
                .iter()
                .map(|(id, name)| format!("[Attachment {id}: \"{}\"]", shown_name(name)))
                .collect::<Vec<_>>()
                .join("\n")
        })
    }

    /// The user content of the turn, without the images' bytes (the turn
    /// adds them after).
    #[must_use]
    pub fn content(&self) -> Content {
        let content = Content::new("user").with_text(self.text.clone());
        match self.attachment_note() {
            Some(note) => content.with_text(note),
            None => content,
        }
    }

    /// The content's text as the request layout names it (its text parts,
    /// one per line: [`super::model::TurnTail::anchor`]).
    #[must_use]
    pub fn anchor(&self) -> String {
        match self.attachment_note() {
            Some(note) => format!("{}\n{note}", self.text),
            None => self.text.clone(),
        }
    }

    /// What a brief of this turn may name beyond the request's messages.
    #[must_use]
    pub fn view(&self) -> BriefView {
        BriefView {
            attachments: self.attachments.clone(),
        }
    }
}

/// What a brief may name on this turn besides the messages of the model's
/// last request: this turn's images, by id and name.
#[derive(Clone, Debug, Default)]
pub struct BriefView {
    attachments: Vec<(String, String)>,
}

impl BriefView {
    /// ADR-0050 decision 9d: every reference of `brief` is in `contents`
    /// (the model's last request) or this turn.
    ///
    /// # Errors
    ///
    /// The refusal code of the first reference that is not.
    pub fn check(&self, contents: &[Content], brief: &Brief) -> Result<CheckedBrief, &'static str> {
        if !brief.excerpts.is_empty() {
            let messages: Vec<(String, String)> = contents.iter().filter_map(quotable).collect();
            for excerpt in &brief.excerpts {
                let quote = normalize_whitespace(&excerpt.quote);
                let seen = !quote.is_empty()
                    && messages.iter().any(|(short, text)| {
                        *short == excerpt.message_id && text.contains(quote.as_str())
                    });
                if !seen {
                    return Err(BRIEF_EXCERPT_NOT_VISIBLE);
                }
            }
        }
        let attachments = brief
            .attachments
            .iter()
            .map(|id| {
                self.attachments
                    .iter()
                    .find(|(known, _)| known == id)
                    .map(|(id, name)| {
                        (
                            id.clone(),
                            format!(
                                "the image \"{}\" attached to this turn (you cannot view it)",
                                shown_name(name)
                            ),
                        )
                    })
                    .ok_or(BRIEF_ATTACHMENT_NOT_READABLE)
            })
            .collect::<Result<Vec<_>, _>>()?;
        // No `recall` on the desktop: no entry was recalled this turn.
        if !brief.memory_entries.is_empty() {
            return Err(BRIEF_MEMORY_NOT_RECALLED);
        }
        // The record names each quoted message by its chat message id, as a
        // cloud spawn's does (#1908); one this process never showed keeps
        // its short id.
        let mut message_ids: Vec<(String, String)> = Vec::new();
        for excerpt in &brief.excerpts {
            if message_ids
                .iter()
                .any(|(short, _)| *short == excerpt.message_id)
            {
                continue;
            }
            if let Some(message) = MessageIds::message_of(&excerpt.message_id) {
                message_ids.push((excerpt.message_id.clone(), message));
            }
        }
        Ok(CheckedBrief {
            attachments,
            memory_entries: Vec::new(),
            message_ids,
        })
    }
}

#[cfg(test)]
#[path = "brief_tests.rs"]
mod tests;
