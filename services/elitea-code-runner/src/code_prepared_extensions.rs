//! Structural parsing only. Supervisor admission and workspace mount proofs own authority.
use serde::Deserialize;
use serde_json::Value;

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct Broker {
    pub(crate) revision: u8,
    pub(crate) policy_sha256: String,
    pub(crate) max_calls: u16,
    pub(crate) max_total_bytes: u32,
}
#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct Workspace {
    revision: u8,
    selection: Selection,
    manifest_sha256: String,
    policy_sha256: String,
}
#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
struct Selection {
    toolkit_id: i32,
    toolkit_reference_sha256: String,
    repository_id: String,
    commit: String,
    mode: String,
    include: Vec<String>,
}
fn hex(value: &str, lengths: &[usize]) -> bool {
    lengths.contains(&value.len())
        && value
            .bytes()
            .all(|v| v.is_ascii_digit() || (b'a'..=b'f').contains(&v))
}
impl Broker {
    pub(crate) fn validate(&self) -> Result<(), &'static str> {
        if self.revision != 1
            || !hex(&self.policy_sha256, &[64])
            || !(1..=4096).contains(&self.max_calls)
            || !(1..=64 * 1024 * 1024).contains(&self.max_total_bytes)
        {
            return Err("Code broker binding is invalid");
        }
        Ok(())
    }
}
impl Workspace {
    fn validate(&self) -> Result<(), &'static str> {
        let s = &self.selection;
        if self.revision != 1
            || !hex(&self.manifest_sha256, &[64])
            || !hex(&self.policy_sha256, &[64])
            || s.toolkit_id <= 0
            || !hex(&s.toolkit_reference_sha256, &[64])
            || !hex(&s.commit, &[40, 64])
            || s.repository_id.is_empty()
            || s.repository_id.len() > 128
            || !s
                .repository_id
                .bytes()
                .all(|v| (0x21..=0x7e).contains(&v) && !b"/\\".contains(&v))
            || !matches!(s.mode.as_str(), "read" | "readwrite")
            || s.include.is_empty()
            || s.include.len() > 128
        {
            return Err("Code workspace binding is invalid");
        }
        for (index, path) in s.include.iter().enumerate() {
            let parts = path.split('/').collect::<Vec<_>>();
            if path.is_empty()
                || path.len() > 1024
                || parts.len() > 64
                || parts.iter().any(|part| {
                    part.is_empty()
                        || matches!(*part, "." | "..")
                        || part.eq_ignore_ascii_case(".git")
                        || part.to_ascii_lowercase().starts_with(".elitea")
                        || !part
                            .bytes()
                            .all(|v| v.is_ascii_alphanumeric() || b"._-+@".contains(&v))
                })
                || index > 0 && path <= &s.include[index - 1]
                || s.include[..index].iter().any(|earlier| {
                    path.strip_prefix(earlier)
                        .is_some_and(|rest| rest.starts_with('/'))
                })
            {
                return Err("Code workspace selection is invalid");
            }
        }
        Ok(())
    }
}

/// Return the underlying legacy dependency revision without stripping signed fields.
pub(crate) fn base_revision(request: &Value) -> Result<u64, &'static str> {
    let revision = request
        .get("revision")
        .and_then(Value::as_u64)
        .ok_or("Code revision is missing")?;
    let broker = request.get("platform_client");
    let workspace = request.get("workspace");
    match (revision, broker, workspace) {
        (1..=3, None, None) => return Ok(revision),
        (4, None, Some(value)) => {
            serde_json::from_value::<Workspace>(value.clone())
                .map_err(|_| "Code workspace binding is invalid")?
                .validate()?;
        }
        (5, Some(value), workspace) => {
            serde_json::from_value::<Broker>(value.clone())
                .map_err(|_| "Code broker binding is invalid")?
                .validate()?;
            if let Some(value) = workspace {
                serde_json::from_value::<Workspace>(value.clone())
                    .map_err(|_| "Code workspace binding is invalid")?
                    .validate()?;
            }
        }
        _ => return Err("Code capability does not match the prepared revision"),
    }
    Ok(if request.get("native_dependencies").is_some() {
        3
    } else if request.get("dependency_bundle_sha256").is_some() {
        2
    } else {
        1
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    fn broker() -> Value {
        serde_json::json!({"revision":1,"policy_sha256":"a".repeat(64),"max_calls":128,"max_total_bytes":16777216})
    }
    fn workspace() -> Value {
        serde_json::json!({"revision":1,"selection":{"toolkit_id":7,"toolkit_reference_sha256":"b".repeat(64),"repository_id":"repo","commit":"c".repeat(40),"mode":"read","include":["src"]},"manifest_sha256":"d".repeat(64),"policy_sha256":"e".repeat(64)})
    }
    #[test]
    fn absent_and_all_extension_combinations_preserve_dependency_revision() {
        for base in 1..=3 {
            let mut request = serde_json::json!({"revision":base});
            if base >= 2 {
                request["dependency_bundle_sha256"] = serde_json::json!("f".repeat(64));
            }
            if base == 3 {
                request["native_dependencies"] = serde_json::json!({"kind":"cargo"});
            }
            assert_eq!(base_revision(&request), Ok(base));
            request["revision"] = serde_json::json!(4);
            request["workspace"] = workspace();
            assert_eq!(base_revision(&request), Ok(base));
            request["revision"] = serde_json::json!(5);
            request["platform_client"] = broker();
            assert_eq!(base_revision(&request), Ok(base));
            request.as_object_mut().unwrap().remove("workspace");
            assert_eq!(base_revision(&request), Ok(base));
        }
    }
    #[test]
    fn capability_downgrade_null_unknown_and_path_changes_are_rejected() {
        for revision in 1..=4 {
            assert!(
                base_revision(&serde_json::json!({"revision":revision,"platform_client":broker()}))
                    .is_err()
            );
        }
        for value in [Value::Null, serde_json::json!({}), serde_json::json!(true)] {
            assert!(
                base_revision(&serde_json::json!({"revision":5,"platform_client":value})).is_err()
            );
        }
        let mut b = broker();
        b["actor_id"] = serde_json::json!(9);
        assert!(base_revision(&serde_json::json!({"revision":5,"platform_client":b})).is_err());
        for path in [
            "../x",
            ".git/config",
            ".elitea-platform/request.cp1",
            "src/.ELITEA-secret",
            "src//x",
            "/src",
        ] {
            let mut w = workspace();
            w["selection"]["include"] = serde_json::json!([path]);
            assert!(base_revision(&serde_json::json!({"revision":4,"workspace":w})).is_err());
        }
        let mut w = workspace();
        w["selection"]["include"] = serde_json::json!(["src", "src/x"]);
        assert!(
            base_revision(
                &serde_json::json!({"revision":5,"platform_client":broker(),"workspace":w})
            )
            .is_err()
        );
    }
}
