//! Who may read a document (ADR-0028 D3).
//!
//! A document is readable by everyone with access to its project
//! ([`Acl::Project`] — a repository, and every source before this ADR), or
//! only by the principals it lists ([`Acl::Restricted`]). A caller is the
//! signed identity the sub-application host verified; group membership is
//! not resolved yet, so a restricted document is visible only to a caller
//! it lists by user id or email — never to an unknown caller (fail closed).

use serde::{Deserialize, Serialize};

/// What a principal names.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum PrincipalKind {
    User,
    Group,
    Email,
}

/// One grantee of a restricted document.
#[derive(Debug, Clone, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub struct Principal {
    pub kind: PrincipalKind,
    pub id: String,
}

/// A document's readers.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "scope", rename_all = "lowercase")]
pub enum Acl {
    /// Anyone with access to the project.
    #[default]
    Project,
    /// Only the listed principals.
    Restricted { principals: Vec<Principal> },
}

/// Who is asking.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Caller {
    /// The platform user id (`X-Elitea-User-Id`, verified by the host).
    pub user_id: Option<String>,
    pub emails: Vec<String>,
    /// Resolved group memberships (empty until group resolution exists).
    pub groups: Vec<String>,
}

impl Acl {
    /// Whether `caller` may read a document with this ACL.
    #[must_use]
    pub fn admits(&self, caller: &Caller) -> bool {
        match self {
            Self::Project => true,
            Self::Restricted { principals } => {
                principals.iter().any(|principal| match principal.kind {
                    PrincipalKind::User => caller.user_id.as_deref() == Some(principal.id.as_str()),
                    PrincipalKind::Email => caller
                        .emails
                        .iter()
                        .any(|email| email.eq_ignore_ascii_case(&principal.id)),
                    PrincipalKind::Group => caller.groups.contains(&principal.id),
                })
            }
        }
    }

    /// Whether this ACL can hide anything from a project member.
    #[must_use]
    pub fn is_restricted(&self) -> bool {
        matches!(self, Self::Restricted { .. })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn restricted(principals: &[(PrincipalKind, &str)]) -> Acl {
        Acl::Restricted {
            principals: principals
                .iter()
                .map(|(kind, id)| Principal {
                    kind: *kind,
                    id: (*id).to_owned(),
                })
                .collect(),
        }
    }

    #[test]
    fn restricted_documents_admit_only_their_principals() {
        let alice = Caller {
            user_id: Some("7".to_owned()),
            emails: vec!["Alice@Example.com".to_owned()],
            groups: vec!["finance".to_owned()],
        };
        assert!(Acl::Project.admits(&Caller::default()));
        assert!(restricted(&[(PrincipalKind::User, "7")]).admits(&alice));
        assert!(restricted(&[(PrincipalKind::Email, "alice@example.com")]).admits(&alice));
        assert!(restricted(&[(PrincipalKind::Group, "finance")]).admits(&alice));
        assert!(!restricted(&[(PrincipalKind::User, "8")]).admits(&alice));
        assert!(!restricted(&[]).admits(&alice), "no principals: nobody");
        assert!(
            !restricted(&[(PrincipalKind::User, "7")]).admits(&Caller::default()),
            "an unknown caller sees no restricted document"
        );
    }

    #[test]
    fn the_stored_form_is_tagged_by_scope() {
        assert_eq!(
            serde_json::to_string(&Acl::Project).unwrap_or_default(),
            r#"{"scope":"project"}"#
        );
        let acl = restricted(&[(PrincipalKind::Group, "g")]);
        let text = serde_json::to_string(&acl).unwrap_or_default();
        assert_eq!(
            text,
            r#"{"scope":"restricted","principals":[{"kind":"group","id":"g"}]}"#
        );
        assert_eq!(serde_json::from_str::<Acl>(&text).ok(), Some(acl));
    }
}
