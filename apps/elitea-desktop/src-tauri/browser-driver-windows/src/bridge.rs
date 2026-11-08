//! The bridge's bookkeeping, in plain Rust (every OS, tested off Windows):
//! which stream owns a pane ([`Panes`]), all-or-nothing subscription
//! ([`subscribe_all`]), and the bytes on their way to the driver
//! ([`Outbox`], [`next_out`]). `com.rs` holds the COM objects in these.

use std::collections::HashMap;

use tokio::sync::mpsc;

use crate::wire;

/// Sees the commands on their way to the page and their answers (the host
/// matches its own trusted input to the reports of the person's, and the
/// page sees an input only once its command was dispatched).
pub trait Watcher: Send + Sync {
    /// A command is about to be sent to the UI thread.
    fn sending(&self, command: &wire::Command);
    /// Command `id` was answered (or refused).
    fn answered(&self, id: u64);
}

/// The host's [`Watcher`].
pub type Observe = std::sync::Arc<dyn Watcher>;

/// A watcher that sees nothing (the conformance run).
#[derive(Clone, Copy, Debug, Default)]
pub struct NoWatcher;

impl Watcher for NoWatcher {
    fn sending(&self, _command: &wire::Command) {}
    fn answered(&self, _id: u64) {}
}

/// The panes whose CDP stream is open, by key (the Tauri webview label).
/// Each [`Panes::insert`] gets a new generation: a stream that closes after
/// its pane was opened again under the same key removes nothing but its
/// own entry ([`Panes::remove`]), and its late commands reach nothing
/// ([`Panes::get`]).
#[derive(Debug)]
pub struct Panes<T> {
    next: u64,
    open: HashMap<String, (u64, T)>,
}

impl<T> Default for Panes<T> {
    fn default() -> Self {
        Self {
            next: 1,
            open: HashMap::new(),
        }
    }
}

impl<T> Panes<T> {
    /// Open `key` with `pane`: its generation, and the pane it replaced
    /// (the caller ends that one's subscriptions).
    pub fn insert(&mut self, key: &str, pane: T) -> (u64, Option<T>) {
        let generation = self.next;
        self.next += 1;
        let old = self.open.insert(key.to_owned(), (generation, pane));
        (generation, old.map(|(_, pane)| pane))
    }

    /// The pane of `key` if it is still that generation's.
    #[must_use]
    pub fn get(&self, key: &str, generation: u64) -> Option<&T> {
        self.open
            .get(key)
            .filter(|(open, _)| *open == generation)
            .map(|(_, pane)| pane)
    }

    /// Close `key`, only if it is still that generation's.
    pub fn remove(&mut self, key: &str, generation: u64) -> Option<T> {
        self.get(key, generation)?;
        self.open.remove(key).map(|(_, pane)| pane)
    }
}

/// Subscribe to each of `names`; on the first failure, undo the ones made
/// (`unsubscribe`) and return the error: no handler is left behind that
/// nothing will remove.
///
/// # Errors
///
/// The first `subscribe` error.
pub fn subscribe_all<S, E>(
    names: &[&'static str],
    mut subscribe: impl FnMut(&'static str) -> Result<S, E>,
    mut unsubscribe: impl FnMut(S),
) -> Result<Vec<S>, E> {
    let mut made = Vec::with_capacity(names.len());
    for &name in names {
        match subscribe(name) {
            Ok(subscription) => made.push(subscription),
            Err(error) => {
                for subscription in made {
                    unsubscribe(subscription);
                }
                return Err(error);
            }
        }
    }
    Ok(made)
}

/// How many screencast frames may wait for the driver. A frame is a whole
/// JPEG (hundreds of KiB): a driver that does not read drops frames, not
/// memory.
pub const FRAMES_WAITING: usize = 4;

/// The bytes on their way to the driver, sent from the UI thread (never
/// blocking it). Answers and events go on an unbounded queue: an answer is
/// never dropped (the driver would time out on it), and each answer is one
/// command the driver sent, so the driver bounds them. Screencast frames go
/// on a bounded one and are dropped when it is full.
#[derive(Clone)]
pub struct Outbox {
    replies: mpsc::UnboundedSender<Vec<u8>>,
    frames: mpsc::Sender<Vec<u8>>,
    watcher: Observe,
}

impl std::fmt::Debug for Outbox {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Outbox").finish_non_exhaustive()
    }
}

/// The reading side of an [`Outbox`].
#[derive(Debug)]
pub struct Inbox {
    replies: mpsc::UnboundedReceiver<Vec<u8>>,
    frames: mpsc::Receiver<Vec<u8>>,
}

/// A new outbox and its inbox. `watcher` sees each command's answer.
#[must_use]
pub fn outbox(watcher: Observe) -> (Outbox, Inbox) {
    let (replies, replies_rx) = mpsc::unbounded_channel();
    let (frames, frames_rx) = mpsc::channel(FRAMES_WAITING);
    (
        Outbox {
            replies,
            frames,
            watcher,
        },
        Inbox {
            replies: replies_rx,
            frames: frames_rx,
        },
    )
}

impl Outbox {
    /// An event: always queued (dropped only when the stream is gone).
    pub fn send(&self, bytes: Vec<u8>) {
        let _ = self.replies.send(bytes);
    }

    /// The answer to command `id`, a reply or a refusal: the watcher sees
    /// it, then it is queued like an event (never dropped).
    pub fn answer(&self, id: u64, bytes: Vec<u8>) {
        self.watcher.answered(id);
        self.send(bytes);
    }

    /// A screencast frame: dropped when [`FRAMES_WAITING`] are waiting.
    /// `false`: it was dropped.
    pub fn frame(&self, bytes: Vec<u8>) -> bool {
        self.frames.try_send(bytes).is_ok()
    }
}

/// The next bytes for the driver, answers and events first. `None`: every
/// outbox is gone.
pub async fn next_out(inbox: &mut Inbox) -> Option<Vec<u8>> {
    tokio::select! {
        biased;
        Some(bytes) = inbox.replies.recv() => Some(bytes),
        Some(bytes) = inbox.frames.recv() => Some(bytes),
        else => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_stream_that_closes_after_its_pane_reopened_leaves_the_new_one() {
        let mut panes = Panes::default();
        let (old, replaced) = panes.insert("browser-t1", "first");
        assert_eq!(replaced, None);
        let (new, replaced) = panes.insert("browser-t1", "second");
        assert_eq!(replaced, Some("first"), "the caller ends its subscriptions");
        assert_ne!(old, new);
        // The first stream's pump ends now: its close and its late commands.
        assert_eq!(panes.remove("browser-t1", old), None);
        assert_eq!(panes.get("browser-t1", old), None);
        assert_eq!(panes.get("browser-t1", new), Some(&"second"));
        assert_eq!(panes.remove("browser-t1", new), Some("second"));
        assert_eq!(panes.get("browser-t1", new), None);
    }

    #[test]
    fn a_failed_subscription_undoes_the_ones_made() {
        let mut removed = Vec::new();
        let made = subscribe_all(
            &["a", "b", "c", "d"],
            |name| {
                if name == "c" {
                    Err("refused")
                } else {
                    Ok(name)
                }
            },
            |name| removed.push(name),
        );
        assert_eq!(made, Err("refused"));
        assert_eq!(removed, ["a", "b"], "nothing is left subscribed");

        let mut removed = Vec::new();
        let made = subscribe_all(&["a", "b"], Ok::<_, ()>, |name| removed.push(name));
        assert_eq!(made, Ok(vec!["a", "b"]));
        assert!(removed.is_empty());
    }

    #[tokio::test]
    async fn frames_are_dropped_when_full_but_answers_never_are_and_go_first() {
        let (out, mut inbox) = outbox(std::sync::Arc::new(NoWatcher));
        for frame in 0..FRAMES_WAITING {
            assert!(out.frame(vec![b'f', u8::try_from(frame).unwrap()]));
        }
        assert!(
            !out.frame(b"late frame".to_vec()),
            "a full queue drops frames"
        );
        for reply in 0..100_u8 {
            out.send(vec![b'r', reply]);
        }
        for reply in 0..100_u8 {
            assert_eq!(next_out(&mut inbox).await, Some(vec![b'r', reply]));
        }
        for frame in 0..FRAMES_WAITING {
            assert_eq!(
                next_out(&mut inbox).await,
                Some(vec![b'f', u8::try_from(frame).unwrap()])
            );
        }
        drop(out);
        assert_eq!(next_out(&mut inbox).await, None);
    }
}

#[cfg(test)]
mod watcher_tests {
    use std::sync::{Arc, Mutex};

    use super::*;

    #[derive(Default)]
    struct Seen(Mutex<Vec<u64>>);

    impl Watcher for Seen {
        fn sending(&self, _command: &wire::Command) {}
        fn answered(&self, id: u64) {
            self.0.lock().unwrap().push(id);
        }
    }

    #[tokio::test]
    async fn the_watcher_sees_each_answer_but_not_events() {
        let seen = Arc::new(Seen::default());
        let (out, mut inbox) = outbox(seen.clone());
        out.send(wire::event("Page.loadEventFired", "{}", ""));
        out.answer(3, wire::reply(3, false, "{}"));
        out.answer(4, wire::refused(4, wire::MALFORMED));
        assert_eq!(*seen.0.lock().unwrap(), [3, 4]);
        for _ in 0..3 {
            assert!(next_out(&mut inbox).await.is_some());
        }
    }
}
