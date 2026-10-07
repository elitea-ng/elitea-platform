//! The foundation every Elitea engine sidecar shares (ADR-0027).
//!
//! * [`errors`] — the error contract on the sidecar socket: the Python
//!   exception names the Go host classifies by, and the category mapping;
//! * [`stream`] — the NDJSON lines a tool run reports and the stop flag
//!   every long call waits on;
//! * [`secret`] — a credential that is zeroed on drop and never formatted;
//! * [`pyjson`] — `json.dumps` byte-for-byte, for outputs the Python engines
//!   defined;
//! * [`pystr`] / [`pyvalue`] — Python string, path and value semantics, for
//!   the same reason.
//!
//! Nothing here knows which engine it serves.

pub mod errors;
pub mod pyjson;
pub mod pystr;
pub mod pyvalue;
pub mod secret;
pub mod stream;
