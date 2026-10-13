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
