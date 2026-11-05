//! Live macOS test of the driver against `TextEdit`, the fixture app every
//! Mac has. Ignored by default: it needs a logged-in GUI session and TCC
//! grants, which no CI runner has.
//!
//! # Running it
//!
//! 1. Grant **Accessibility** (and, for the capture check, **Screen
//!    Recording**) to the app that runs `cargo`: System Settings > Privacy &
//!    Security > Accessibility (Screen Recording), then add your terminal
//!    (Terminal, iTerm, Ghostty, the IDE). Restart the terminal after the
//!    grant; TCC reads it at launch.
//! 2. Close any open `TextEdit` documents you care about. The test opens a new
//!    untitled document, types into it and leaves it open; close it without
//!    saving afterwards.
//! 3. From `libs/rust`:
//!
//!    ```sh
//!    CARGO_TARGET_DIR=~/projects/eliteaai/_scratch/rust-target-1581 \
//!      cargo test -p elitea-desktop-control --test macos_live -- --ignored --nocapture --test-threads=1
//!    ```
//!
//! Do not touch the keyboard or the mouse while it runs: the driver types
//! into the frontmost window.
//!
//! What it proves: the apps and windows lists see `TextEdit`; a snapshot of
//! its window has refs bound to that snapshot and respects the node budget;
//! a ref from a forgotten snapshot fails as `stale_ref`; typing into the
//! document shows up in a later snapshot; a denied chord (`cmd+q`) is
//! refused before anything is sent; the synthetic marker classifies.

#![cfg(target_os = "macos")]

use std::process::Command;
use std::time::Duration;

use elitea_desktop_control::{
    Action, DesktopDriver, DesktopError, Grant, InputOrigin, KeyChord, MacDriver, Node,
    SnapshotBudget, WaitCondition, WindowTarget,
};

const TEXTEDIT: &str = "com.apple.TextEdit";

fn find<'a>(node: &'a Node, pred: &dyn Fn(&Node) -> bool) -> Option<&'a Node> {
    if pred(node) {
        return Some(node);
    }
    node.children.iter().find_map(|c| find(c, pred))
}

fn refs_in(node: &Node) -> usize {
    usize::from(node.element_ref.is_some()) + node.children.iter().map(refs_in).sum::<usize>()
}

#[test]
#[ignore = "needs a GUI session with Accessibility granted; see the module docs"]
fn textedit_round_trip() {
    let driver = MacDriver::new();
    assert_eq!(
        driver.permissions().accessibility,
        Grant::Granted,
        "grant Accessibility to the terminal that runs cargo (module docs)"
    );

    // A fresh untitled document, frontmost.
    let status = Command::new("/usr/bin/open")
        .args(["-n", "-b", TEXTEDIT])
        .status()
        .expect("open TextEdit");
    assert!(status.success());
    driver
        .wait(
            &WaitCondition::Window {
                bundle_id: TEXTEDIT.into(),
                title_contains: None,
            },
            Duration::from_secs(10),
        )
        .expect("a TextEdit window appears");
    let _ = Command::new("/usr/bin/osascript")
        .args([
            "-e",
            "tell application \"TextEdit\" to make new document",
            "-e",
            "tell application \"TextEdit\" to activate",
        ])
        .status();
    std::thread::sleep(Duration::from_millis(800));

    let apps = driver.list_apps().expect("apps");
    assert!(
        apps.iter()
            .any(|a| a.bundle_id.as_deref() == Some(TEXTEDIT))
    );
    let windows = driver.list_windows(Some(TEXTEDIT)).expect("windows");
    assert_ne!(windows.len(), 0);

    // A small budget is respected and marked.
    let small = driver
        .snapshot(
            &WindowTarget::App(TEXTEDIT.into()),
            &SnapshotBudget {
                max_nodes: 3,
                ..SnapshotBudget::default()
            },
        )
        .expect("small snapshot");
    assert!(small.node_count <= 3);
    assert!(
        small.truncated,
        "a 3-node budget must cut a TextEdit window"
    );

    let snap = driver
        .snapshot(
            &WindowTarget::App(TEXTEDIT.into()),
            &SnapshotBudget::default(),
        )
        .expect("snapshot");
    assert!(refs_in(&snap.root) > 0, "the window has actionable nodes");
    let area = find(&snap.root, &|n| {
        n.role == "textfield" && n.element_ref.is_some()
    })
    .expect("the document's text area has a ref");
    let area_ref = area.element_ref.clone().expect("ref");
    assert_eq!(area_ref.snapshot(), &snap.id);

    // Type into the document through the ref, then see it in a new snapshot.
    let marker = "elitea desktop-control live test";
    driver
        .act(&area_ref, &Action::Focus)
        .expect("focus the text area");
    driver
        .type_text(Some(&area_ref), marker)
        .expect("type into the text area");
    driver
        .wait(
            &WaitCondition::Text {
                target: WindowTarget::App(TEXTEDIT.into()),
                text: marker.into(),
            },
            Duration::from_secs(5),
        )
        .expect("the typed text appears");

    // Denied chords never reach the app.
    let err = driver
        .key(
            &WindowTarget::App(TEXTEDIT.into()),
            &KeyChord::parse("cmd+q").expect("chord"),
        )
        .unwrap_err();
    assert!(matches!(err, DesktopError::ChordRefused(_)), "{err:?}");

    // A forgotten snapshot's refs are stale.
    driver.forget_snapshots();
    let err = driver.act(&area_ref, &Action::Focus).unwrap_err();
    assert_eq!(err.code(), "stale_ref", "{err:?}");

    // The marker the driver stamps classifies as synthetic.
    let marker = driver.synthetic_marker();
    assert_eq!(
        driver.is_synthetic_event(marker.value()),
        InputOrigin::Synthetic
    );
    assert_eq!(driver.is_synthetic_event(0), InputOrigin::Physical);
}
