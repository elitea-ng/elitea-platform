//! Main-owned saved-family reference for the Code authority port.
//! This module contains no family registration or HTTP dispatcher.
use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct SavedChildScopeRef {
    pub(crate) scope_id: String,
    pub(crate) revision: u64,
    pub(crate) digest_sha256: String,
}
impl SavedChildScopeRef {
    pub(crate) fn validate(&self) -> bool {
        valid_digest(&self.scope_id) && self.revision == 1 && valid_digest(&self.digest_sha256)
    }
}

fn valid_digest(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}
