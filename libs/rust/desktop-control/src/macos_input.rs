//! The macOS input watch (ADR-0047 decision 6): a listen-only event tap at
//! the annotated session, on a thread of its own, feeding
//! [`crate::input::classify`].
//!
//! - **Listen only.** The tap never drops, changes or delays an event; it
//!   reads the type, the source user-data field (the driver's marker), the
//!   target process, the key code and the modifiers.
//! - **The annotated session** is where events carry their target process,
//!   so input aimed at Elitea's own windows (the session panel's buttons)
//!   is told apart and is not a pause.
//! - **The grant.** A listen-only tap needs the Input Monitoring grant (or
//!   Accessibility, which computer use requires anyway). Without it the OS
//!   refuses the tap and the watch fails with `permission_denied`; the host
//!   then refuses every `ComputerAct` (no pause could be honoured).
//! - **A disabled tap** (the OS disables a tap that is slow, or during
//!   secure input) is re-enabled at once.
//!
//! The safe `core-graphics` API is used throughout: this crate has no
//! `unsafe` code. The tap is made, run and dropped on its own thread (it is
//! not `Send`); the handle sets a stop flag, wakes that thread's run loop
//! and joins the thread, so no signal is heard after the drop returns.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, mpsc};
use std::thread::JoinHandle;
use std::time::Duration;

use core_foundation::base::TCFType as _;
use core_foundation::runloop::CFRunLoop;
use core_foundation::string::CFString;
use core_graphics::event::{
    CGEventFlags, CGEventTap, CGEventTapLocation, CGEventTapOptions, CGEventTapPlacement,
    CGEventType, CallbackResult, EventField,
};

use crate::input::{Held, InputEvent, InputFilter, InputKind, InputWatch, SignalSink, classify};
use crate::{DesktopError, SyntheticMarker};

/// One pass of the tap's run loop at most: the longest a drop waits.
const PASS: Duration = Duration::from_millis(200);

/// The event types the tap listens to.
fn interesting() -> Vec<CGEventType> {
    vec![
        CGEventType::KeyDown,
        CGEventType::LeftMouseDown,
        CGEventType::RightMouseDown,
        CGEventType::OtherMouseDown,
        CGEventType::LeftMouseDragged,
        CGEventType::RightMouseDragged,
        CGEventType::OtherMouseDragged,
        CGEventType::ScrollWheel,
    ]
}

fn kind_of(event_type: CGEventType) -> InputKind {
    match event_type {
        CGEventType::KeyDown => InputKind::KeyDown,
        CGEventType::FlagsChanged => InputKind::FlagsChanged,
        CGEventType::LeftMouseDown | CGEventType::RightMouseDown | CGEventType::OtherMouseDown => {
            InputKind::MouseDown
        }
        CGEventType::LeftMouseDragged
        | CGEventType::RightMouseDragged
        | CGEventType::OtherMouseDragged => InputKind::MouseDragged,
        CGEventType::MouseMoved => InputKind::MouseMoved,
        CGEventType::ScrollWheel => InputKind::Scroll,
        _ => InputKind::Other,
    }
}

fn held(flags: CGEventFlags) -> Held {
    Held {
        control: flags.contains(CGEventFlags::CGEventFlagControl),
        option: flags.contains(CGEventFlags::CGEventFlagAlternate),
        command: flags.contains(CGEventFlags::CGEventFlagCommand),
        shift: flags.contains(CGEventFlags::CGEventFlagShift),
    }
}

/// A run loop mode by its name. The CoreFoundation constants
/// (`kCFRunLoopCommonModes`, `kCFRunLoopDefaultMode`) are these strings and
/// modes compare by value; naming them avoids reading an extern static.
fn mode(name: &'static str) -> CFString {
    CFString::from_static_string(name)
}

/// A running tap: its thread, that thread's run loop and its stop flag.
struct MacWatch {
    run_loop: CFRunLoop,
    stop: Arc<AtomicBool>,
    thread: Option<JoinHandle<()>>,
}

impl InputWatch for MacWatch {}

impl Drop for MacWatch {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        self.run_loop.stop();
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
    }
}

/// Start the watch: each of the person's inputs reaches `sink` as a
/// [`crate::input::Signal`].
pub(crate) fn watch(
    marker: SyntheticMarker,
    filter: InputFilter,
    sink: SignalSink,
) -> Result<Box<dyn InputWatch>, DesktopError> {
    let (ready, started) = mpsc::channel::<Result<CFRunLoop, DesktopError>>();
    let stop = Arc::new(AtomicBool::new(false));
    let stopping = stop.clone();
    let thread = std::thread::Builder::new()
        .name("elitea-computer-input".into())
        .spawn(move || run(marker, &filter, &sink, &stopping, &ready))
        .map_err(|e| DesktopError::Internal(format!("input watch thread: {e}")))?;
    match started.recv() {
        Ok(Ok(run_loop)) => Ok(Box::new(MacWatch {
            run_loop,
            stop,
            thread: Some(thread),
        })),
        Ok(Err(error)) => {
            let _ = thread.join();
            Err(error)
        }
        Err(_) => {
            let _ = thread.join();
            Err(DesktopError::Internal("the input watch stopped".into()))
        }
    }
}

fn run(
    marker: SyntheticMarker,
    filter: &InputFilter,
    sink: &SignalSink,
    stop: &AtomicBool,
    ready: &mpsc::Sender<Result<CFRunLoop, DesktopError>>,
) {
    // The callback cannot reach the tap it belongs to, so a disabled tap is
    // re-enabled from the run loop: the callback flags it and ends the
    // loop's current pass.
    let disabled = Arc::new(AtomicBool::new(false));
    let flag = disabled.clone();
    let run_loop = CFRunLoop::get_current();
    let waker = run_loop.clone();
    let filter = filter.clone();
    // The sink is shared with the tap's callback for the tap's life only;
    // both end on this thread.
    let (signals, heard) = mpsc::channel();
    let tap = CGEventTap::new(
        CGEventTapLocation::AnnotatedSession,
        CGEventTapPlacement::TailAppendEventTap,
        CGEventTapOptions::ListenOnly,
        interesting(),
        move |_proxy, event_type, event| {
            if matches!(
                event_type,
                CGEventType::TapDisabledByTimeout | CGEventType::TapDisabledByUserInput
            ) {
                flag.store(true, Ordering::SeqCst);
                waker.stop();
                return CallbackResult::Keep;
            }
            let field = |name| event.get_integer_value_field(name);
            let input = InputEvent {
                kind: kind_of(event_type),
                source_user_data: field(EventField::EVENT_SOURCE_USER_DATA),
                target_pid: u32::try_from(field(EventField::EVENT_TARGET_UNIX_PROCESS_ID))
                    .unwrap_or(0),
                keycode: u16::try_from(field(EventField::KEYBOARD_EVENT_KEYCODE)).unwrap_or(0),
                held: held(event.get_flags()),
            };
            if let Some(signal) = classify(&input, marker, &filter) {
                let _ = signals.send(signal);
                waker.stop();
            }
            CallbackResult::Keep
        },
    );
    let Ok(tap) = tap else {
        let _ = ready.send(Err(DesktopError::PermissionDenied(
            "Elitea may not watch the keyboard and pointer (Accessibility or Input Monitoring)"
                .into(),
        )));
        return;
    };
    let Ok(source) = tap.mach_port().create_runloop_source(0) else {
        let _ = ready.send(Err(DesktopError::Internal(
            "the input watch has no run loop source".into(),
        )));
        return;
    };
    let common = mode("kCFRunLoopCommonModes");
    let default = mode("kCFRunLoopDefaultMode");
    run_loop.add_source(&source, common.as_concrete_TypeRef());
    tap.enable();
    if ready.send(Ok(run_loop.clone())).is_err() {
        return;
    }
    while !stop.load(Ordering::SeqCst) {
        CFRunLoop::run_in_mode(default.as_concrete_TypeRef(), PASS, false);
        // Signals are delivered outside the OS callback, so a slow sink can
        // never get the tap disabled.
        while let Ok(signal) = heard.try_recv() {
            if !stop.load(Ordering::SeqCst) {
                sink(signal);
            }
        }
        if disabled.swap(false, Ordering::SeqCst) {
            tap.enable();
        }
    }
    run_loop.remove_source(&source, common.as_concrete_TypeRef());
}
