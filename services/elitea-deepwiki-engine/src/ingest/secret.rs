//! A credential that cannot leak through `Debug`, `Display` or a log line.
//!
//! The Python engine kept tokens as plain `str` and wrote them into clone
//! URLs, so they reached `argv`, `.git/config` and error text (ADR-0026,
//! security context). Here a credential lives only inside [`Secret`]: it
//! has no `Display`, its `Debug` prints a placeholder, and its memory is
//! zeroed when it drops. The one way to read it is [`Secret::expose`],
//! which the clone transport calls to build the `Authorization` header.

use std::fmt;
use zeroize::Zeroizing;

/// A secret string (a token, a password, or a ready `Authorization` value).
#[derive(Clone, PartialEq, Eq)]
pub struct Secret(Zeroizing<String>);

impl Secret {
    /// Wrap `value`. An empty value is not a credential, so it gives `None`.
    #[must_use]
    pub fn new(value: String) -> Option<Self> {
        if value.is_empty() {
            None
        } else {
            Some(Self(Zeroizing::new(value)))
        }
    }

    /// The secret text. Call this only where the value leaves the process
    /// on the wire (the clone transport), never to format a message.
    #[must_use]
    pub fn expose(&self) -> &str {
        &self.0
    }
}

impl fmt::Debug for Secret {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("Secret(<redacted>)")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn debug_never_shows_the_value() {
        let secret = Secret::new("ghp_supersecret".to_owned());
        let shown = format!("{secret:?}");
        assert!(!shown.contains("supersecret"), "{shown}");
        assert_eq!(shown, "Some(Secret(<redacted>))");
    }

    #[test]
    fn an_empty_value_is_no_secret() {
        assert!(Secret::new(String::new()).is_none());
    }
}
