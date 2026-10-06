//! [`SharedStr`]: an immutable string many nodes or edges share.

use std::fmt;
use std::ops::Deref;
use std::sync::Arc;

/// An immutable string that clones without copying: the path of a file
/// that every node of that file carries, the file stem of every edge from
/// it. The empty string holds no allocation.
#[derive(Clone, Default, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub struct SharedStr(Option<Arc<str>>);

impl SharedStr {
    #[must_use]
    pub fn as_str(&self) -> &str {
        self.0.as_deref().unwrap_or("")
    }
}

impl Deref for SharedStr {
    type Target = str;

    fn deref(&self) -> &str {
        self.as_str()
    }
}

impl AsRef<str> for SharedStr {
    fn as_ref(&self) -> &str {
        self.as_str()
    }
}

impl From<&str> for SharedStr {
    fn from(text: &str) -> Self {
        Self((!text.is_empty()).then(|| Arc::from(text)))
    }
}

impl From<String> for SharedStr {
    fn from(text: String) -> Self {
        Self((!text.is_empty()).then(|| Arc::from(text)))
    }
}

impl From<&String> for SharedStr {
    fn from(text: &String) -> Self {
        Self::from(text.as_str())
    }
}

impl PartialEq<str> for SharedStr {
    fn eq(&self, other: &str) -> bool {
        self.as_str() == other
    }
}

impl PartialEq<&str> for SharedStr {
    fn eq(&self, other: &&str) -> bool {
        self.as_str() == *other
    }
}

impl PartialEq<String> for SharedStr {
    fn eq(&self, other: &String) -> bool {
        self.as_str() == other
    }
}

impl fmt::Debug for SharedStr {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        fmt::Debug::fmt(self.as_str(), f)
    }
}

impl fmt::Display for SharedStr {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn clones_share_and_empty_holds_nothing() {
        let a = SharedStr::from("pkg/a.py");
        let b = a.clone();
        assert_eq!(b, "pkg/a.py");
        assert!(
            a.0.as_ref()
                .zip(b.0.as_ref())
                .is_some_and(|(x, y)| Arc::ptr_eq(x, y))
        );
        assert!(SharedStr::from("").0.is_none());
        assert_eq!(SharedStr::default().as_str(), "");
        assert_eq!(format!("{a}"), "pkg/a.py");
    }
}
