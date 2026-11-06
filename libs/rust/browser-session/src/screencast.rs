//! The view stream (ADR-0049 decision 6): JPEG frames from
//! `Page.startScreencast`, paced for the view relay (#1597).
//!
//! * **Latest frame first.** The stream holds one slot. A frame that arrives
//!   before the viewer took the previous one replaces it; a slow viewer sees
//!   the newest page, never a backlog.
//! * **Ack pacing.** The engine sends the next frame only after the
//!   previous one is acknowledged (`Page.screencastFrameAck`). The ack is
//!   sent when the frame is taken (so the engine never runs ahead of the
//!   viewer) and no sooner than [`MIN_FRAME_INTERVAL`] after the previous
//!   ack: at most the viewer's `max_fps`, never more than [`MAX_FPS`]
//!   frames per second.
//! * **No stream without a viewer.** The screencast starts with the first
//!   [`Screencast::subscribe`] and stops when the last [`Viewer`] is
//!   dropped.
//!
//! There is no network endpoint here. The agent's line protocol
//! ([`crate::protocol`]) sends each frame to the supervisor as a `frame`
//! line and takes the next one only after the supervisor's `ack`; the view
//! relay (#1597) carries them on from there.

use std::sync::{Arc, Mutex, MutexGuard, PoisonError};
use std::time::Duration;

use base64::Engine as _;
use serde_json::{Value, json};
use tokio::sync::{Notify, broadcast};
use tokio::time::Instant;

use crate::cdp::{Connection, Event};

/// At most this many frames per second (ADR-0049 decision 6).
pub const MAX_FPS: u32 = 10;
/// The shortest gap between two acknowledgements, at [`MAX_FPS`].
pub const MIN_FRAME_INTERVAL: Duration = Duration::from_millis(1000 / MAX_FPS as u64);
/// JPEG quality of a frame.
pub const JPEG_QUALITY: u32 = 70;

/// One view frame.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Frame {
    /// From 1, in the order the engine sent them; a gap is a frame replaced
    /// before anyone took it.
    pub sequence: u64,
    /// JPEG bytes.
    pub jpeg: Vec<u8>,
    /// The page's CSS size the frame shows.
    pub width: u32,
    pub height: u32,
}

#[derive(Default)]
struct Slot {
    latest: Option<Frame>,
    /// The engine's id of the frame waiting for its ack.
    unacked: Option<i64>,
    last_ack: Option<Instant>,
    sequence: u64,
    viewers: usize,
    /// The last sequence each viewer took, by viewer id.
    taken: Vec<(u64, u64)>,
    next_viewer: u64,
    running: bool,
}

struct Shared {
    cdp: Connection,
    session: String,
    width: u32,
    height: u32,
    slot: Mutex<Slot>,
    arrived: Notify,
}

impl Shared {
    fn slot(&self) -> MutexGuard<'_, Slot> {
        self.slot.lock().unwrap_or_else(PoisonError::into_inner)
    }
}

/// The page's screencast. Cheap to clone.
#[derive(Clone)]
pub struct Screencast {
    shared: Arc<Shared>,
}

impl Screencast {
    /// A stream of `session`'s page, at most `width` by `height`. `events`
    /// is a receiver of the connection's events.
    #[must_use]
    pub fn new(
        cdp: Connection,
        session: String,
        width: u32,
        height: u32,
        events: broadcast::Receiver<Arc<Event>>,
    ) -> Self {
        let shared = Arc::new(Shared {
            cdp,
            session,
            width,
            height,
            slot: Mutex::new(Slot::default()),
            arrived: Notify::new(),
        });
        tokio::spawn(receive(Arc::clone(&shared), events));
        Self { shared }
    }

    /// Attach a viewer that takes at most `max_fps` frames a second (1 to
    /// [`MAX_FPS`]); the first viewer starts the screencast.
    ///
    /// # Errors
    ///
    /// The engine refused `Page.startScreencast`.
    pub async fn subscribe(&self, max_fps: u32) -> Result<Viewer, crate::cdp::CdpError> {
        let interval = Duration::from_millis(1000 / u64::from(max_fps.clamp(1, MAX_FPS)));
        let (id, start) = {
            let mut slot = self.shared.slot();
            slot.viewers += 1;
            slot.next_viewer += 1;
            let id = slot.next_viewer;
            slot.taken.push((id, 0));
            let start = !slot.running;
            slot.running = true;
            (id, start)
        };
        let viewer = Viewer {
            shared: Arc::clone(&self.shared),
            id,
            interval,
        };
        if start {
            self.shared
                .cdp
                .call(
                    Some(&self.shared.session),
                    "Page.startScreencast",
                    json!({
                        "format": "jpeg",
                        "quality": JPEG_QUALITY,
                        "maxWidth": self.shared.width,
                        "maxHeight": self.shared.height,
                        "everyNthFrame": 1,
                    }),
                )
                .await?;
        }
        Ok(viewer)
    }

    /// Whether the engine is streaming (a viewer is attached).
    #[must_use]
    pub fn is_running(&self) -> bool {
        self.shared.slot().running
    }
}

/// One attached viewer. Dropping the last one stops the screencast.
pub struct Viewer {
    shared: Arc<Shared>,
    id: u64,
    interval: Duration,
}

impl Viewer {
    /// The newest frame this viewer has not taken, waiting for one. `None`
    /// when the engine is gone.
    pub async fn next(&mut self) -> Option<Frame> {
        loop {
            let notified = self.shared.arrived.notified();
            let (frame, ack) = {
                let mut slot = self.shared.slot();
                let taken = slot
                    .taken
                    .iter()
                    .find(|(id, _)| *id == self.id)
                    .map_or(0, |(_, sequence)| *sequence);
                match slot.latest.clone() {
                    Some(frame) if frame.sequence > taken => {
                        if let Some(entry) = slot.taken.iter_mut().find(|(id, _)| *id == self.id) {
                            entry.1 = frame.sequence;
                        }
                        (
                            Some(frame),
                            slot.unacked.take().map(|id| (id, slot.last_ack)),
                        )
                    }
                    _ => (None, None),
                }
            };
            if let Some((frame_id, last_ack)) = ack {
                pace_and_ack(&self.shared, frame_id, last_ack, self.interval).await;
            }
            if let Some(frame) = frame {
                return Some(frame);
            }
            if !self.shared.cdp.is_open() {
                return None;
            }
            notified.await;
        }
    }
}

impl Drop for Viewer {
    fn drop(&mut self) {
        let stop = {
            let mut slot = self.shared.slot();
            slot.viewers = slot.viewers.saturating_sub(1);
            slot.taken.retain(|(id, _)| *id != self.id);
            let stop = slot.viewers == 0 && slot.running;
            if stop {
                slot.running = false;
                slot.latest = None;
                slot.unacked = None;
            }
            stop
        };
        if stop {
            let shared = Arc::clone(&self.shared);
            tokio::spawn(async move {
                let _ = shared
                    .cdp
                    .call(Some(&shared.session), "Page.stopScreencast", json!({}))
                    .await;
            });
        }
    }
}

/// Acknowledge `frame_id`, no sooner than `interval` after the previous
/// acknowledgement.
async fn pace_and_ack(
    shared: &Shared,
    frame_id: i64,
    last_ack: Option<Instant>,
    interval: Duration,
) {
    if let Some(last) = last_ack {
        tokio::time::sleep_until(last + interval).await;
    }
    shared.slot().last_ack = Some(Instant::now());
    let _ = shared
        .cdp
        .call(
            Some(&shared.session),
            "Page.screencastFrameAck",
            json!({ "sessionId": frame_id }),
        )
        .await;
}

async fn receive(shared: Arc<Shared>, mut events: broadcast::Receiver<Arc<Event>>) {
    loop {
        let event = match events.recv().await {
            Ok(event) => event,
            Err(broadcast::error::RecvError::Lagged(_)) => continue,
            Err(broadcast::error::RecvError::Closed) => break,
        };
        if event.method != "Page.screencastFrame"
            || event.session_id.as_deref() != Some(shared.session.as_str())
        {
            continue;
        }
        if let Some(frame) = decode(&event.params) {
            let mut slot = shared.slot();
            if !slot.running {
                continue;
            }
            slot.sequence += 1;
            let sequence = slot.sequence;
            slot.latest = Some(Frame {
                sequence,
                ..frame.0
            });
            slot.unacked = Some(frame.1);
        }
        shared.arrived.notify_waiters();
    }
    shared.arrived.notify_waiters();
}

/// A `Page.screencastFrame`'s frame and its ack id.
fn decode(params: &Value) -> Option<(Frame, i64)> {
    let jpeg = base64::engine::general_purpose::STANDARD
        .decode(params["data"].as_str()?)
        .ok()?;
    let metadata = &params["metadata"];
    #[allow(clippy::cast_possible_truncation, clippy::cast_sign_loss)]
    let size = |name: &str| {
        metadata[name]
            .as_f64()
            .map_or(0, |value| value.max(0.0).round() as u32)
    };
    Some((
        Frame {
            sequence: 0,
            jpeg,
            width: size("deviceWidth"),
            height: size("deviceHeight"),
        },
        params["sessionId"].as_i64()?,
    ))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};

    /// A fake engine: it records acks and lets the test push frames.
    struct FakeEngine {
        acks: Arc<AtomicUsize>,
        starts: Arc<AtomicUsize>,
        stops: Arc<AtomicUsize>,
        push: tokio::sync::mpsc::UnboundedSender<Value>,
    }

    fn fake() -> (Connection, FakeEngine) {
        let (client_write, engine_read) = tokio::io::duplex(1 << 20);
        let (mut engine_write, client_read) = tokio::io::duplex(1 << 20);
        let acks = Arc::new(AtomicUsize::new(0));
        let starts = Arc::new(AtomicUsize::new(0));
        let stops = Arc::new(AtomicUsize::new(0));
        let (push, mut pushed) = tokio::sync::mpsc::unbounded_channel::<Value>();
        let (counts, ack_count) = ((Arc::clone(&starts), Arc::clone(&stops)), Arc::clone(&acks));
        let (out, mut out_rx) = tokio::sync::mpsc::unbounded_channel::<Value>();
        tokio::spawn(async move {
            loop {
                let message = tokio::select! {
                    Some(message) = pushed.recv() => message,
                    Some(message) = out_rx.recv() => message,
                    else => break,
                };
                let mut bytes = serde_json::to_vec(&message).unwrap();
                bytes.push(0);
                if engine_write.write_all(&bytes).await.is_err() {
                    break;
                }
            }
        });
        tokio::spawn(async move {
            let mut reader = BufReader::new(engine_read);
            let mut record = Vec::new();
            while reader.read_until(0, &mut record).await.unwrap_or(0) > 0 {
                record.pop();
                let command: Value = serde_json::from_slice(&record).unwrap();
                record.clear();
                match command["method"].as_str() {
                    Some("Page.screencastFrameAck") => {
                        ack_count.fetch_add(1, Ordering::SeqCst);
                    }
                    Some("Page.startScreencast") => {
                        counts.0.fetch_add(1, Ordering::SeqCst);
                    }
                    Some("Page.stopScreencast") => {
                        counts.1.fetch_add(1, Ordering::SeqCst);
                    }
                    _ => {}
                }
                let _ = out.send(json!({"id": command["id"], "result": {}}));
            }
        });
        let connection = Connection::new(client_read, client_write);
        (
            connection,
            FakeEngine {
                acks,
                starts,
                stops,
                push,
            },
        )
    }

    fn frame_event(id: i64, byte: u8) -> Value {
        json!({
            "method": "Page.screencastFrame",
            "sessionId": "S",
            "params": {
                "data": base64::engine::general_purpose::STANDARD.encode([byte]),
                "sessionId": id,
                "metadata": {"deviceWidth": 1280.0, "deviceHeight": 800.0},
            },
        })
    }

    #[tokio::test]
    async fn latest_frame_first_with_ack_pacing() {
        let (cdp, engine) = fake();
        let cast = Screencast::new(cdp.clone(), "S".to_owned(), 1280, 800, cdp.events());
        assert!(!cast.is_running());
        let mut viewer = cast.subscribe(MAX_FPS).await.unwrap();
        assert!(cast.is_running());
        assert_eq!(engine.starts.load(Ordering::SeqCst), 1);
        // Three frames before the viewer looks: it gets only the newest.
        for (id, byte) in [(1, 1), (2, 2), (3, 3)] {
            engine.push.send(frame_event(id, byte)).unwrap();
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
        let frame = viewer.next().await.unwrap();
        assert_eq!(frame.jpeg, vec![3]);
        assert_eq!(frame.sequence, 3);
        assert_eq!((frame.width, frame.height), (1280, 800));
        // The next frame is acked no sooner than 100 ms after the first ack.
        engine.push.send(frame_event(4, 4)).unwrap();
        let started = Instant::now();
        let frame = viewer.next().await.unwrap();
        assert_eq!(frame.jpeg, vec![4]);
        assert!(started.elapsed() + Duration::from_millis(5) >= MIN_FRAME_INTERVAL);
        tokio::time::sleep(Duration::from_millis(20)).await;
        assert_eq!(engine.acks.load(Ordering::SeqCst), 2);
        // The last viewer leaving stops the stream.
        drop(viewer);
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert!(!cast.is_running());
        assert_eq!(engine.stops.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn frames_decode_or_are_skipped() {
        let (frame, id) = decode(&frame_event(7, 9)["params"]).unwrap();
        assert_eq!((frame.jpeg, id), (vec![9], 7));
        assert!(decode(&json!({"data": "%%%", "sessionId": 1})).is_none());
    }
}
