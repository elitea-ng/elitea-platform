//! The cloud browser session agent (ADR-0049 decisions 2 and 4).
//!
//! The binary inside the browser session pod. It starts the engine
//! (chrome-headless-shell, `full`), drives it over CDP, and implements the
//! `elitea-browser-tools` contract on it, so the cloud agent sees the same
//! browser tools as the desktop one:
//!
//! * [`cdp`] — a thin CDP client over the engine's debugging **pipe**
//!   (`--remote-debugging-pipe`): no TCP or WebSocket listener exists, so
//!   CDP cannot leave the process pair;
//! * [`engine`] — the engine's flags: the egress proxy as its only route out,
//!   QUIC, non-proxied WebRTC and DNS over HTTPS off, `--no-sandbox` only
//!   under gVisor;
//! * [`page`] — the event task: navigations, loads, the network, frames,
//!   proxy authentication, dialogs and popups, and the page's own refused
//!   navigations;
//! * [`ax`] — Chromium's accessibility tree to the contract's raw nodes;
//! * [`driver`] — [`driver::CdpDriver`], the `BrowserDriver` over CDP, with
//!   ADR-0048 decision 5's hard limits;
//! * [`screencast`] — the paced view stream (latest frame first, ack paced,
//!   at most 10 frames a second);
//! * [`control`] — who drives (ADR-0048 decision 4);
//! * [`protocol`] and [`session`] — the supervisor's line protocol
//!   (`libs/proto/elitea/runtime/v1/browser_session_agent_v1.md`) over a
//!   [`protocol::Backend`], and the backend that runs the driver.
#![cfg_attr(
    not(test),
    deny(
        clippy::expect_used,
        clippy::panic,
        clippy::todo,
        clippy::unimplemented,
        clippy::unwrap_used
    )
)]

pub mod ax;
pub mod cdp;
pub mod control;
pub mod driver;
pub mod engine;
pub mod keys;
pub mod page;
pub mod protocol;
pub mod screencast;
// The pod's backend starts the engine on its debugging pipe (unix). The
// desktop's Windows driver uses `driver::CdpDriver::attach` over `WebView2`'s
// own CDP and needs neither.
#[cfg(unix)]
pub mod session;
