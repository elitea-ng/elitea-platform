//! Readable fault lines (FU-299).
//!
//! - **Codes.** `build.rs` reads every `libs/go/faults/registry/*.json` (the
//!   Go package owns the format) and generates [`codes`] and [`REGISTRY`].
//!   [`Code`] has a private constructor, so only registered codes exist: a
//!   typo is a compile error.
//! - **[`Fault`]** is implemented by an error that has a registered code.
//! - **Cause chains, safe by type.** [`cause_chain`] walks `source()` (at most
//!   [`MAX_CAUSES`] elements of at most [`MAX_CAUSE_BYTES`] each) and renders
//!   every element through a [`Renderer`] list. A type nobody registered is
//!   never formatted: Rust cannot name the concrete type behind a
//!   `dyn Error`, so it renders as [`UNREGISTERED_CAUSE`]. Registering a type
//!   with [`render_type_name`] renders its type name only, with
//!   [`render_safe`] its [`SafeCause`] text, and with [`render_fault`] its
//!   code and sentence. "Never format error values" stays true for anything
//!   not listed.
//! - **[`fault!`]** emits one tracing event per fault: `msg` is the code's
//!   registry sentence, plus `code`, `action`, `retryable` (when the fault
//!   knows it), `cause` and the caller's fields. The event's target is the
//!   caller's module, so the caller's log filter applies.
//! - **[`OperatorJsonLayer`]** writes the operator stream: one JSON object per
//!   event, span fields flattened, no span open or close lines.

use std::error::Error;
use std::fmt;

mod operator_stream;

pub use operator_stream::OperatorJsonLayer;

#[cfg(any(test, feature = "logtest"))]
pub mod logtest;

/// Re-exports the [`fault!`] macro expands to. Not a public API.
#[doc(hidden)]
pub mod __private {
    pub use tracing;
}

/// Most elements a cause chain renders.
pub const MAX_CAUSES: usize = 8;
/// Most bytes one rendered cause element keeps.
pub const MAX_CAUSE_BYTES: usize = 512;
/// What a cause of an unregistered type renders as.
pub const UNREGISTERED_CAUSE: &str = "unregistered error type";

/// Who reads a code's sentence.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Audience {
    Operator,
    User,
    Both,
}

/// Whether trying again can succeed.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Retryable {
    Yes,
    No,
    NotApplicable,
}

/// One registry entry. Built only by the generated code.
#[derive(Debug)]
pub struct Entry {
    code: &'static str,
    sentence: &'static str,
    action: &'static str,
    audience: Audience,
    retryable: Retryable,
    owner: &'static str,
    runtime_kind: Option<&'static str>,
    wire_code: Option<&'static str>,
}

impl Entry {
    #[must_use]
    pub const fn code(&self) -> &'static str {
        self.code
    }

    #[must_use]
    pub const fn sentence(&self) -> &'static str {
        self.sentence
    }

    #[must_use]
    pub const fn action(&self) -> &'static str {
        self.action
    }

    #[must_use]
    pub const fn audience(&self) -> Audience {
        self.audience
    }

    #[must_use]
    pub const fn retryable(&self) -> Retryable {
        self.retryable
    }

    #[must_use]
    pub const fn owner(&self) -> &'static str {
        self.owner
    }

    /// The Rust `RuntimeFailureKind` variant a `runtime.*` entry mirrors.
    #[must_use]
    pub const fn runtime_kind(&self) -> Option<&'static str> {
        self.runtime_kind
    }

    /// The `RuntimeErrorCodeV1` name a `runtime.*` entry is sent under.
    #[must_use]
    pub const fn wire_code(&self) -> Option<&'static str> {
        self.wire_code
    }
}

/// A registered code. Only the generated [`codes`] constants and
/// [`Code::lookup`] produce one.
#[derive(Clone, Copy)]
pub struct Code(&'static Entry);

impl Code {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        self.0.code
    }

    /// The registry sentence: what happened and what happens next.
    #[must_use]
    pub const fn sentence(self) -> &'static str {
        self.0.sentence
    }

    /// What an operator or user does about it.
    #[must_use]
    pub const fn action(self) -> &'static str {
        self.0.action
    }

    #[must_use]
    pub const fn entry(self) -> &'static Entry {
        self.0
    }

    /// The registered code spelled `code`, if there is one.
    #[must_use]
    pub fn lookup(code: &str) -> Option<Self> {
        REGISTRY.iter().find(|entry| entry.code == code).map(Self)
    }
}

impl PartialEq for Code {
    fn eq(&self, other: &Self) -> bool {
        self.as_str() == other.as_str()
    }
}

impl Eq for Code {}

impl std::hash::Hash for Code {
    fn hash<H: std::hash::Hasher>(&self, state: &mut H) {
        self.as_str().hash(state);
    }
}

impl fmt::Debug for Code {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

impl fmt::Display for Code {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

#[allow(clippy::all, clippy::pedantic)]
mod generated {
    use super::{Audience, Entry, Retryable};

    include!(concat!(env!("OUT_DIR"), "/registry.rs"));
}

pub use generated::{REGISTRY, codes};

/// An error with a registered code.
pub trait Fault: Error {
    fn fault_code(&self) -> Code;

    /// Whether retrying can succeed, when this error knows.
    fn fault_retryable(&self) -> Option<bool> {
        None
    }
}

/// Renders one error safely: only data the type's author has checked is free
/// of request data, secrets, paths and URLs.
pub trait SafeCause {
    fn safe_cause(&self) -> String;
}

/// `std::io::Error` renders its `ErrorKind` only: the message can hold a path.
impl SafeCause for std::io::Error {
    fn safe_cause(&self) -> String {
        format!("io: {:?}", self.kind())
    }
}

/// Renders an element of a cause chain if it is of the type it knows.
pub type Renderer = fn(&(dyn Error + 'static)) -> Option<String>;

/// The renderers every caller gets: `std::io::Error` by kind.
pub const DEFAULT_RENDERERS: &[Renderer] = &[render_safe::<std::io::Error>];

/// Renders a `T` through its [`SafeCause`].
#[must_use]
pub fn render_safe<T: SafeCause + Error + 'static>(
    error: &(dyn Error + 'static),
) -> Option<String> {
    error.downcast_ref::<T>().map(SafeCause::safe_cause)
}

/// Renders a `T` as `<code>: <sentence>`.
#[must_use]
pub fn render_fault<T: Fault + 'static>(error: &(dyn Error + 'static)) -> Option<String> {
    error.downcast_ref::<T>().map(|fault| {
        let code = fault.fault_code();
        format!("{}: {}", code.as_str(), code.sentence())
    })
}

/// Renders a `T` as its type name only, for a type whose text is not safe.
#[must_use]
pub fn render_type_name<T: Error + 'static>(error: &(dyn Error + 'static)) -> Option<String> {
    error
        .downcast_ref::<T>()
        .map(|_| std::any::type_name::<T>().to_owned())
}

/// The cause chain of `error`, outermost first, `error` itself included.
#[must_use]
pub fn cause_chain(error: &(dyn Error + 'static), renderers: &[Renderer]) -> Vec<String> {
    chain_from(Some(error), renderers)
}

/// The causes of `error` (its `source()` chain), outermost first, rendered as
/// a JSON array of strings: the `cause` field of a fault line.
#[must_use]
pub fn source_chain_json<E: Error + ?Sized>(error: &E, renderers: &[Renderer]) -> String {
    let chain = chain_from(error.source(), renderers);
    serde_json::to_string(&chain).unwrap_or_else(|_| String::from("[]"))
}

fn chain_from(mut next: Option<&(dyn Error + 'static)>, renderers: &[Renderer]) -> Vec<String> {
    let mut chain = Vec::new();
    while let Some(error) = next {
        if chain.len() == MAX_CAUSES {
            break;
        }
        let rendered = renderers
            .iter()
            .find_map(|render| render(error))
            .unwrap_or_else(|| UNREGISTERED_CAUSE.to_owned());
        chain.push(bounded(rendered));
        next = error.source();
    }
    chain
}

fn bounded(mut text: String) -> String {
    if text.len() > MAX_CAUSE_BYTES {
        let mut end = MAX_CAUSE_BYTES;
        while !text.is_char_boundary(end) {
            end -= 1;
        }
        text.truncate(end);
    }
    text
}

/// Emits one fault line for `error` (a `&T` where `T: Fault`).
///
/// ```ignore
/// elitea_faults::fault!(tracing::Level::WARN, &error, renderers = SAFE_CAUSES, event = "x");
/// elitea_faults::fault!(tracing::Level::WARN, code = codes::X, event = "x");
/// ```
///
/// The line's message is the registry sentence; `code`, `action`,
/// `retryable` (when the fault knows it) and `cause` (its `source()` chain,
/// made safe by `renderers`, [`DEFAULT_RENDERERS`] when omitted) are added
/// before the caller's fields. The `code = …` form is for an outcome with a
/// registered code and no error value. Fields take no trailing comma.
#[macro_export]
macro_rules! fault {
    ($level:expr, code = $code:expr $(, $($field:tt)+)?) => {{
        let __fault_code: $crate::Code = $code;
        $crate::__private::tracing::event!(
            $level,
            code = __fault_code.as_str(),
            action = __fault_code.action(),
            $($($field)+,)?
            "{}",
            __fault_code.sentence()
        );
    }};
    ($level:expr, $error:expr, renderers = $renderers:expr $(, $($field:tt)+)?) => {{
        #[allow(unused_imports)]
        use $crate::Fault as _;
        let __fault_error = $error;
        let __fault_code: $crate::Code = __fault_error.fault_code();
        let __fault_cause = $crate::source_chain_json(__fault_error, $renderers);
        $crate::__private::tracing::event!(
            $level,
            code = __fault_code.as_str(),
            action = __fault_code.action(),
            retryable = __fault_error.fault_retryable(),
            cause = __fault_cause.as_str(),
            $($($field)+,)?
            "{}",
            __fault_code.sentence()
        );
    }};
    ($level:expr, $error:expr $(, $($field:tt)+)?) => {
        $crate::fault!($level, $error, renderers = $crate::DEFAULT_RENDERERS $(, $($field)+)?)
    };
}

#[cfg(test)]
mod tests;
