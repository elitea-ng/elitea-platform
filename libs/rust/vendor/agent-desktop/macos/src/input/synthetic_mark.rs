// Elitea patch 2 (libs/rust/vendor/README.md, "agent-desktop"): every
// CGEvent this crate creates carries a known value in the
// `kCGEventSourceUserData` field, so an event tap in the host can tell
// the driver's own synthetic input apart from the person's physical input
// (ADR-0047 decision 6, the user-active guard).
//
// Upstream has no such marker. A physical event from the HID system leaves the
// field at zero, so the tag is never zero.

use std::sync::atomic::{AtomicI64, Ordering};

/// The tag stamped on synthetic events until the host sets its own.
pub const DEFAULT_SYNTHETIC_EVENT_TAG: i64 = 0x454C_4954_4541_4443;

static TAG: AtomicI64 = AtomicI64::new(DEFAULT_SYNTHETIC_EVENT_TAG);

/// Sets the tag stamped on every later synthetic event. Zero is refused,
/// because zero is what a physical event carries.
pub fn set_synthetic_event_tag(tag: i64) -> bool {
    if tag == 0 {
        return false;
    }
    TAG.store(tag, Ordering::Relaxed);
    true
}

/// The tag stamped on synthetic events now.
pub fn synthetic_event_tag() -> i64 {
    TAG.load(Ordering::Relaxed)
}

#[cfg(target_os = "macos")]
pub(crate) fn mark(event: &core_graphics::event::CGEvent) {
    event.set_integer_value_field(
        core_graphics::event::EventField::EVENT_SOURCE_USER_DATA,
        synthetic_event_tag(),
    );
}
