//! Tests that count permits on the process-wide gate (the self-exit signal,
//! xlsx parsing under the guard). Their own binary, and the tests take turns:
//! nothing else may hold a permit meanwhile.
#![allow(clippy::pedantic)]
mod common;
use common::*;
use std::time::{Duration, Instant};

use elitea_office_core::testing::hold_permit;
use elitea_office_core::{CellValue, Limits, OfficeError, Workbook, health};

static TURN: std::sync::Mutex<()> = std::sync::Mutex::new(());
fn turn() -> std::sync::MutexGuard<'static, ()> {
    TURN.lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

#[test]
fn a_permit_held_past_three_deadlines_reports_stuck_until_the_thread_exits() {
    let _t = turn();
    wait_idle();
    assert!(!health().stuck());
    let l = Limits {
        max_concurrent: 2,
        ..Limits::default()
    };
    // A library call that will not give its thread back: 20 ms deadline,
    // 400 ms of work.
    let handle = hold_permit(&l, Duration::from_millis(400), Duration::from_millis(20)).unwrap();
    assert_eq!(health().active, 1);
    assert!(!health().stuck(), "not yet three deadlines");
    let start = Instant::now();
    while !health().stuck() && start.elapsed() < Duration::from_secs(2) {
        std::thread::sleep(Duration::from_millis(5));
    }
    let h = health();
    assert!(
        h.stuck() && h.degraded && h.longest_held >= Duration::from_millis(60),
        "{h:?}"
    );
    // The caller got its deadline error and returned; the thread did not.
    handle.join().unwrap();
    assert_eq!(
        health().active,
        1,
        "the abandoned thread still holds its permit"
    );
    let start = Instant::now();
    while health().active > 0 && start.elapsed() < Duration::from_secs(5) {
        std::thread::sleep(Duration::from_millis(5));
    }
    // The permit comes back when the thread exits, and the signal clears.
    assert_eq!(health().active, 0);
    assert!(!health().stuck());
}

fn wait_idle() {
    let start = Instant::now();
    while health().active > 0 && start.elapsed() < Duration::from_secs(10) {
        std::thread::sleep(Duration::from_millis(5));
    }
}

// ---- finding 1: every xlsx parse runs on the parse thread -----------------

fn big_sheet() -> Vec<u8> {
    let l = limits();
    let mut w = Workbook::new(&l);
    for r in 0..600u32 {
        for c in 0..500u16 {
            w.write_cell(
                0,
                r,
                c,
                &CellValue::Number(f64::from(r * 500 + u32::from(c))),
            )
            .unwrap();
        }
    }
    w.save().unwrap()
}

#[test]
fn xlsx_open_takes_a_permit_and_obeys_the_deadline() {
    let _t = turn();
    wait_idle();
    let bytes = big_sheet();
    // The open (validate + parse of every sheet) is a guarded call: with the
    // only permit held it is Busy, and with a 1 ms deadline it is cut off.
    let one = Limits {
        max_concurrent: 1,
        ..Limits::default()
    };
    let held = hold_permit(&one, Duration::from_millis(400), Duration::from_secs(5)).unwrap();
    assert!(matches!(
        Workbook::open(&bytes, &one),
        Err(OfficeError::Busy)
    ));
    held.join().unwrap();
    let tight = Limits {
        call_deadline: Duration::from_millis(1),
        max_concurrent: 64,
        ..Limits::default()
    };
    assert!(matches!(
        Workbook::open(&bytes, &tight),
        Err(OfficeError::DeadlineExceeded)
    ));
}

#[test]
fn xlsx_reads_after_open_need_no_permit_and_see_every_sheet() {
    let _t = turn();
    wait_idle();
    // All sheets were parsed inside open(): a later read never parses, so it
    // works while every permit is held.
    let l = limits();
    let w = Workbook::open(&big_sheet(), &l).unwrap();
    let one = Limits {
        max_concurrent: 1,
        ..Limits::default()
    };
    let held = hold_permit(&one, Duration::from_millis(300), Duration::from_secs(5)).unwrap();
    assert_eq!(
        w.read_cell(0, 599, 499).unwrap(),
        CellValue::Number(299_999.0)
    );
    held.join().unwrap();
}

// ---- review round 2: the process-wide fault and delay hooks live here, in the
// binary whose tests take turns -------------------------------------------------

use elitea_office_core::testing::{
    delay_next_guarded_call, fail_next_paragraph_mapping, fail_next_replace,
};
use elitea_office_core::{Deck, Document, SlideLayout};

const REAL_PPTX: &[u8] =
    include_bytes!("../../../../services/elitea-main/internal/infra/extract/testdata/deck.pptx");

fn texts(d: &Deck) -> Vec<String> {
    (0..d.slide_count().unwrap())
        .flat_map(|s| d.shapes(s).unwrap().into_iter().map(|x| x.text))
        .collect()
}

// ---- 6: a failed replace restores; a failed restore poisons; authored decks refused

#[test]
fn a_failed_replace_restores_the_deck() {
    let _t = turn();
    wait_idle();
    let mut d = Deck::open(REAL_PPTX, &limits()).unwrap();
    let before = texts(&d);
    fail_next_replace(false);
    let r = d.replace_text("FIXTURETOKENPPTX", "X");
    assert!(
        matches!(r, Err(OfficeError::Internal(ref m)) if m.contains("injected")),
        "{r:?}"
    );
    assert_eq!(texts(&d), before, "restored from the snapshot");
    // And the deck is still fully usable.
    assert_eq!(d.replace_text("FIXTURETOKENPPTX", "X").unwrap(), 2);
    assert!(texts(&d).iter().any(|t| t.contains("X1")));
}

#[test]
fn a_failed_restore_poisons_the_deck() {
    let _t = turn();
    wait_idle();
    let mut d = Deck::open(REAL_PPTX, &limits()).unwrap();
    let saved = d.save().unwrap();
    fail_next_replace(true);
    let r = d.replace_text("FIXTURETOKENPPTX", "X");
    assert!(
        matches!(r, Err(OfficeError::Internal(ref m)) if m.contains("state lost")),
        "{r:?}"
    );
    // Every later call says so, whichever kind of call it is.
    for r in [
        d.slide_count().map(|_| ()),
        d.save().map(|_| ()),
        d.to_markdown().map(|_| ()),
        d.replace_text("a", "b").map(|_| ()),
        d.set_title(0, "t"),
    ] {
        assert!(
            matches!(r, Err(OfficeError::Internal(ref m)) if m.contains("state lost; reopen from saved bytes")),
            "{r:?}"
        );
    }
    // Reopening from saved bytes is the way out.
    assert!(Deck::open(&saved, &limits()).unwrap().slide_count().is_ok());
}

#[test]
fn replace_text_refuses_a_deck_with_session_authored_state() {
    let _t = turn();
    wait_idle();
    let l = limits();
    let mut d = Deck::open(REAL_PPTX, &l).unwrap();
    d.add_slide(SlideLayout::Blank).unwrap();
    assert!(
        matches!(d.replace_text("FIXTURE", "x"), Err(OfficeError::Unsupported(m)) if m.contains("save and reopen"))
    );
    // Save, reopen, and it works.
    let mut again = Deck::open(&d.save().unwrap(), &l).unwrap();
    assert_eq!(again.replace_text("FIXTURETOKENPPTX1", "x").unwrap(), 1);
    // A table counts as authored state too.
    let mut t = Deck::open(REAL_PPTX, &l).unwrap();
    t.add_table(
        0,
        1,
        1,
        elitea_office_core::Rect::inches(0.0, 0.0, 1.0, 1.0),
    )
    .unwrap();
    assert!(matches!(
        t.replace_text("FIXTURE", "x"),
        Err(OfficeError::Unsupported(_))
    ));
}

// ---- 7: the paragraph tag never survives a failure -----------------------------------

#[test]
fn a_failed_paragraph_mapping_leaves_no_marker() {
    let _t = turn();
    wait_idle();
    let l = limits();
    let mut d = Document::new(&l);
    d.add_paragraph("one").unwrap();
    d.add_paragraph("two").unwrap();
    let before = d.to_plain_text().unwrap();
    let saved_before = d.save().unwrap();
    fail_next_paragraph_mapping();
    let r = d.set_paragraph_text(1, "TWO");
    assert!(
        matches!(r, Err(OfficeError::Internal(ref m)) if m.contains("injected")),
        "{r:?}"
    );
    let after = d.to_plain_text().unwrap();
    assert_eq!(after, before);
    assert!(!after.contains('\u{E000}') && !after.contains("marker"));
    let saved_after = d.save().unwrap();
    let reopened = Document::open(&saved_after, &limits())
        .unwrap()
        .to_plain_text()
        .unwrap();
    assert!(!reopened.contains('\u{E000}') && !reopened.contains("marker"));
    let _ = saved_before;
    // A later edit works and leaves no tag either.
    d.set_paragraph_text(1, "TWO").unwrap();
    let text = d.to_plain_text().unwrap();
    assert!(text.contains("TWO") && !text.contains('\u{E000}'));
}

// ---- 8: a timed-out mutating call poisons its document ------------------------------------

#[test]
fn a_timed_out_edit_makes_the_deck_indeterminate() {
    let _t = turn();
    wait_idle();
    let l = Limits {
        call_deadline: Duration::from_millis(300),
        max_concurrent: 4,
        ..Limits::default()
    };
    let mut d = Deck::open(REAL_PPTX, &l).unwrap();
    delay_next_guarded_call(1200);
    let r = d.replace_text("FIXTURETOKENPPTX", "X");
    assert!(matches!(r, Err(OfficeError::DeadlineExceeded)), "{r:?}");
    // While the thread still runs, and after it has finished.
    assert!(matches!(d.slide_count(), Err(OfficeError::Indeterminate)));
    assert!(matches!(d.save(), Err(OfficeError::Indeterminate)));
    assert!(
        health().active >= 1,
        "the abandoned thread is still running"
    );
    wait_idle();
    assert!(matches!(d.slide_count(), Err(OfficeError::Indeterminate)));
    assert!(matches!(d.to_markdown(), Err(OfficeError::Indeterminate)));
    assert!(matches!(
        d.replace_text("a", "b"),
        Err(OfficeError::Indeterminate)
    ));
}

#[test]
fn a_timed_out_read_leaves_the_deck_alone() {
    let _t = turn();
    wait_idle();
    let l = Limits {
        call_deadline: Duration::from_millis(300),
        max_concurrent: 4,
        ..Limits::default()
    };
    let d = Deck::open(REAL_PPTX, &l).unwrap();
    delay_next_guarded_call(1000);
    assert!(matches!(
        d.to_markdown(),
        Err(OfficeError::DeadlineExceeded)
    ));
    wait_idle();
    assert_eq!(d.slide_count().unwrap(), 2);
    assert!(d.to_markdown().unwrap().contains("Opening slide"));
}

#[test]
fn a_timed_out_paragraph_edit_makes_the_document_indeterminate() {
    let _t = turn();
    wait_idle();
    let l = Limits {
        call_deadline: Duration::from_millis(300),
        max_concurrent: 4,
        ..Limits::default()
    };
    let mut d = Document::new(&l);
    d.add_paragraph("one").unwrap();
    delay_next_guarded_call(1000);
    assert!(matches!(
        d.set_paragraph_text(0, "x"),
        Err(OfficeError::DeadlineExceeded)
    ));
    assert!(matches!(d.to_plain_text(), Err(OfficeError::Indeterminate)));
    assert!(matches!(
        d.add_paragraph("y"),
        Err(OfficeError::Indeterminate)
    ));
    wait_idle();
    assert!(matches!(d.save(), Err(OfficeError::Indeterminate)));
}
