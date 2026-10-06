//! Shared Main registration selector for an actual admitted saved family.
//! A missing provider on a scoped runtime is a denial, never root authority.

use async_trait::async_trait;
use serde::{Deserialize, Serialize};

use super::saved_child_http::SavedChildScopeRef;
use crate::agents::graph::http_action::HttpActionError;

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub(crate) enum SavedChildPurpose {
    NestedHttp,
    CodeRecovery,
    CodeDebug,
    CodeWorkspace,
    PlatformBroker,
}
impl SavedChildPurpose {
    pub(crate) fn as_str(self) -> &'static str {
        match self {
            Self::NestedHttp => "nested_http",
            Self::CodeRecovery => "code_recovery",
            Self::CodeDebug => "code_debug",
            Self::CodeWorkspace => "code_workspace",
            Self::PlatformBroker => "platform_broker",
        }
    }
}

/// Implementations retain the nonserialized actual family capability and Main's
/// canonical registration receipt. Consumers supply a visit, never a project,
/// actor, decoded event receipt, or replacement definition.
#[async_trait]
pub(crate) trait SavedChildScopeProvider: Send + Sync {
    /// Read-only relation selection for a newly admitted family whose actual
    /// containing thread is already in this exact owner. It grants no operation
    /// purpose and may not translate an unknown descendant into root authority.
    async fn for_registration_parent(
        &self,
        _thread_id: &str,
    ) -> Result<SavedChildScopeRef, HttpActionError> {
        Err(HttpActionError::PolicyDenied)
    }

    async fn for_visit(
        &self,
        thread_id: &str,
        node_id: &str,
        purpose: SavedChildPurpose,
    ) -> Result<SavedChildScopeRef, HttpActionError>;
}

#[cfg(test)]
mod tests {
    use super::*;

    struct VisitOnlyProvider;
    #[async_trait]
    impl SavedChildScopeProvider for VisitOnlyProvider {
        async fn for_visit(
            &self,
            _thread: &str,
            _node: &str,
            _purpose: SavedChildPurpose,
        ) -> Result<SavedChildScopeRef, HttpActionError> {
            Err(HttpActionError::PolicyDenied)
        }
    }
    #[tokio::test]
    async fn ancestor_relation_requires_owning_provider_implementation() {
        assert!(matches!(
            VisitOnlyProvider
                .for_registration_parent("same-name-child")
                .await,
            Err(HttpActionError::PolicyDenied)
        ));
    }

    #[test]
    fn purposes_have_exact_main_registered_names() {
        for purpose in [
            SavedChildPurpose::NestedHttp,
            SavedChildPurpose::CodeRecovery,
            SavedChildPurpose::CodeDebug,
            SavedChildPurpose::CodeWorkspace,
            SavedChildPurpose::PlatformBroker,
        ] {
            let bytes = serde_json::to_vec(&purpose).unwrap();
            assert_eq!(bytes, format!("\"{}\"", purpose.as_str()).as_bytes());
            assert_eq!(
                serde_json::from_slice::<SavedChildPurpose>(&bytes).unwrap(),
                purpose
            );
        }
        assert!(serde_json::from_str::<SavedChildPurpose>("\"code\"").is_err());
    }
}
