//! Platform notifications (the web UI's bell, fed over SSE) reaching the OS
//! while the window is in the background: a native notification for a new
//! one, and the unread count on the Dock badge next to the approvals and
//! waiting cloud threads (`attention.rs`).
//!
//! The page is the only one that sees the platform's notification stream, so
//! it asks; the host decides. The text is the deployment's, so it is
//! length-checked, stripped of control and bidi characters and shortened
//! before the OS shows it, and nothing is shown while the window is in front.
//!
//! The page also says which notification a notice is for. The host shows
//! one notice per notification, and none for a `run_finished` (ADR-0045
//! decision 5) that did not run in the cloud: a chain that ran on this
//! computer was already noticed when its last turn ended (`attention.rs`).

use std::collections::{HashSet, VecDeque};
use std::sync::{Arc, Mutex};

use tauri::State;

use crate::attention::Attention;

/// The longest title the page may send, in characters.
pub const MAX_TITLE: usize = 120;
/// The longest body the page may send, in characters.
pub const MAX_BODY: usize = 1000;
/// The most characters of the body a notification shows (with the `…`).
pub const SHOWN_BODY: usize = 120;
/// The largest unread count the badge carries.
pub const MAX_UNREAD: u32 = 999;
/// Notification ids remembered as already shown; past this the oldest are dropped.
const MAX_REMEMBERED: usize = 512;
/// The longest notification id the page may name, in characters.
const MAX_ID: usize = 64;

/// Why a notice was refused. Its text is what the page gets.
#[derive(Debug, thiserror::Error, Eq, PartialEq)]
pub enum NoticeError {
    #[error("a notice title is at most {MAX_TITLE} characters")]
    TitleTooLong,
    #[error("a notice body is at most {MAX_BODY} characters")]
    BodyTooLong,
    #[error("a notice needs a title")]
    EmptyTitle,
}

/// Bidi embeddings, overrides and isolates (U+202A–U+202E, U+2066–U+2069):
/// they can make a notification read differently from what it says.
fn is_bidi_control(c: char) -> bool {
    matches!(c, '\u{202A}'..='\u{202E}' | '\u{2066}'..='\u{2069}')
}

/// Control and bidi characters dropped, whitespace runs (a dropped newline or
/// tab counts as one) collapsed to one space, trimmed.
#[must_use]
pub fn sanitize(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut gap = false;
    for c in text.chars() {
        if c.is_whitespace() || c.is_control() {
            // A newline or tab separates words; other controls just vanish.
            gap |= c.is_whitespace();
            continue;
        }
        if is_bidi_control(c) {
            continue;
        }
        if gap && !out.is_empty() {
            out.push(' ');
        }
        gap = false;
        out.push(c);
    }
    out
}

/// `text` cut to `max` characters, the last one an `…` when it was longer.
fn shorten(text: &str, max: usize) -> String {
    if text.chars().count() <= max {
        return text.to_owned();
    }
    let mut out: String = text.chars().take(max - 1).collect();
    out.truncate(out.trim_end().len());
    out.push('…');
    out
}

/// The title and body a notification shows, or why the page's were refused.
/// The limits apply to what the page sent, before anything else.
///
/// # Errors
///
/// A title over [`MAX_TITLE`] or a body over [`MAX_BODY`] characters, or a
/// title that is empty once sanitized.
pub fn notice_text(title: &str, body: &str) -> Result<(String, String), NoticeError> {
    if title.chars().count() > MAX_TITLE {
        return Err(NoticeError::TitleTooLong);
    }
    if body.chars().count() > MAX_BODY {
        return Err(NoticeError::BodyTooLong);
    }
    let title = sanitize(title);
    if title.is_empty() {
        return Err(NoticeError::EmptyTitle);
    }
    Ok((title, shorten(&sanitize(body), SHOWN_BODY)))
}

/// The notifications already shown, so that each one shows once.
#[derive(Default)]
pub struct NoticeFilter {
    shown: Mutex<Shown>,
}

#[derive(Default)]
struct Shown {
    ids: HashSet<String>,
    order: VecDeque<String>,
}

impl NoticeFilter {
    /// Whether a notice for this notification may show, remembering it if so.
    ///
    /// - A `run_finished` shows only when its `origin` is `cloud` (a cloud
    ///   chain or a handed-off thread). Any other origin, or none, is this
    ///   computer's own chain, already noticed by the host.
    /// - A notification `id` shows once. A notice without an id (an older
    ///   page) is not remembered.
    pub fn admit(&self, id: Option<&str>, event_type: Option<&str>, origin: Option<&str>) -> bool {
        if event_type == Some("run_finished") && origin != Some("cloud") {
            return false;
        }
        let Some(id) = id.filter(|id| !id.is_empty() && id.chars().count() <= MAX_ID) else {
            return true;
        };
        let mut shown = self
            .shown
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if !shown.ids.insert(id.to_owned()) {
            return false;
        }
        shown.order.push_back(id.to_owned());
        if shown.order.len() > MAX_REMEMBERED
            && let Some(oldest) = shown.order.pop_front()
        {
            shown.ids.remove(&oldest);
        }
        true
    }
}

/// The unread count the badge carries.
#[must_use]
pub fn clamp_unread(unread: u32) -> usize {
    // u32 → usize is lossless on every target Tauri builds for.
    usize::try_from(unread.min(MAX_UNREAD)).unwrap_or(0)
}

/// Show a platform notification while the window is not in front.
///
/// `notification_id`, `event_type` and `origin` name the notification (see
/// [`NoticeFilter::admit`]); all three are optional.
///
/// # Errors
///
/// The title or body is too long, or the title is empty (see
/// [`notice_text`]). Not showing it (window in front, permission refused,
/// shown before, a local `run_finished`) is not an error.
// Async so it runs off the main thread: the first notice may ask the OS for
// permission, as `Attention::notify` does off the emitting thread.
#[tauri::command(rename_all = "snake_case")]
#[allow(clippy::needless_pass_by_value)] // tauri's extractors and owned args
pub async fn platform_notice_show(
    attention: State<'_, Arc<Attention>>,
    filter: State<'_, NoticeFilter>,
    title: String,
    body: String,
    notification_id: Option<String>,
    event_type: Option<String>,
    origin: Option<String>,
) -> Result<(), String> {
    let (title, body) = notice_text(&title, &body).map_err(|e| e.to_string())?;
    if !filter.admit(
        notification_id.as_deref(),
        event_type.as_deref(),
        origin.as_deref(),
    ) {
        return Ok(());
    }
    attention.show_unfocused(&title, &body);
    Ok(())
}

/// The number of unread platform notifications changed.
#[tauri::command(rename_all = "snake_case")]
#[allow(clippy::needless_pass_by_value)] // tauri's extractors
pub fn platform_notice_badge(attention: State<'_, Arc<Attention>>, unread: u32) {
    attention.set_platform_unread(clamp_unread(unread));
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn notice_sanitize_strips_controls_and_bidi_and_collapses_whitespace() {
        assert_eq!(
            sanitize(
                "  New\r\n\tcomment \u{0}on\u{7}  \u{202E}gpj.exe\u{202C} \u{2066}x\u{2069}  "
            ),
            "New comment on gpj.exe x"
        );
        assert_eq!(sanitize("a\u{1b}[31mb"), "a[31mb");
        assert_eq!(sanitize(" \n\t "), "");
        assert_eq!(sanitize("Ünïcode — ok"), "Ünïcode — ok");
    }

    #[test]
    fn notice_rejects_over_long_title_and_body_by_chars() {
        // 120 multi-byte chars pass: the limit counts characters, not bytes.
        let title = "é".repeat(MAX_TITLE);
        assert!(notice_text(&title, "").is_ok());
        assert_eq!(
            notice_text(&"é".repeat(MAX_TITLE + 1), ""),
            Err(NoticeError::TitleTooLong)
        );
        assert!(notice_text("t", &"ж".repeat(MAX_BODY)).is_ok());
        assert_eq!(
            notice_text("t", &"ж".repeat(MAX_BODY + 1)),
            Err(NoticeError::BodyTooLong)
        );
        // Checked before sanitizing: padding with controls does not sneak past.
        assert_eq!(
            notice_text(&format!("t{}", "\n".repeat(MAX_TITLE)), ""),
            Err(NoticeError::TitleTooLong)
        );
    }

    #[test]
    fn notice_rejects_an_empty_title() {
        assert_eq!(notice_text("", "b"), Err(NoticeError::EmptyTitle));
        assert_eq!(
            notice_text(" \n\u{202E}\t\u{0} ", "b"),
            Err(NoticeError::EmptyTitle)
        );
    }

    #[test]
    fn notice_body_is_truncated_with_an_ellipsis() {
        let (_, body) = notice_text("t", &"x".repeat(500)).unwrap();
        assert_eq!(body.chars().count(), SHOWN_BODY);
        assert!(body.ends_with('…'));
        let exact = "y".repeat(SHOWN_BODY);
        assert_eq!(notice_text("t", &exact).unwrap().1, exact);
        let (title, body) = notice_text(" Mentioned ", "  a\n b ").unwrap();
        assert_eq!((title.as_str(), body.as_str()), ("Mentioned", "a b"));
    }

    #[test]
    fn notice_badge_count_is_clamped_and_summed() {
        assert_eq!(clamp_unread(0), 0);
        assert_eq!(clamp_unread(7), 7);
        assert_eq!(clamp_unread(u32::MAX), 999);
        assert_eq!(crate::attention::badge_count(2, 1, 3), 6);
        assert_eq!(crate::attention::badge_count(0, 0, 0), 0);
        assert_eq!(
            crate::attention::badge_count(usize::MAX, 1, 1),
            usize::MAX,
            "saturates"
        );
    }

    #[test]
    fn notice_run_finished_shows_for_cloud_origin_only() {
        let filter = NoticeFilter::default();
        assert!(filter.admit(Some("1"), Some("run_finished"), Some("cloud")));
        assert!(!filter.admit(Some("2"), Some("run_finished"), Some("desktop")));
        assert!(
            !filter.admit(Some("3"), Some("run_finished"), None),
            "no origin is not the cloud"
        );
        // Other kinds do not depend on an origin.
        assert!(filter.admit(Some("4"), Some("chat_user_mentioned"), None));
        // A dropped local row was not remembered as shown.
        assert!(filter.admit(Some("2"), Some("chat_user_mentioned"), None));
    }

    #[test]
    fn notice_shows_once_per_notification() {
        let filter = NoticeFilter::default();
        assert!(filter.admit(Some("7"), Some("run_finished"), Some("cloud")));
        assert!(!filter.admit(Some("7"), Some("run_finished"), Some("cloud")));
        assert!(filter.admit(Some("8"), Some("run_finished"), Some("cloud")));
        // Without an id (an older page) every notice shows.
        assert!(filter.admit(None, None, None));
        assert!(filter.admit(None, None, None));
        // An over-long id is treated as none: shown, not remembered.
        let long = "x".repeat(MAX_ID + 1);
        assert!(filter.admit(Some(&long), None, None));
        assert!(filter.admit(Some(&long), None, None));
    }

    #[test]
    fn notice_filter_forgets_the_oldest_past_its_bound() {
        let filter = NoticeFilter::default();
        for id in 0..=MAX_REMEMBERED {
            assert!(filter.admit(Some(&id.to_string()), None, None));
        }
        // "0" was dropped to make room; the newest is still remembered.
        assert!(filter.admit(Some("0"), None, None));
        assert!(!filter.admit(Some(&MAX_REMEMBERED.to_string()), None, None));
    }
}
