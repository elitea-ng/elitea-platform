//! Order-explicit JSON encoding for digests and rendered values.
//!
//! `serde_json::to_vec` writes an object's members in the map's iteration
//! order. Without `serde_json`'s `preserve_order` feature that order is the
//! `BTreeMap` order (keys sorted by their bytes); with it, it is insertion
//! order. Cargo unifies features per build, so linking any crate that enables
//! `preserve_order` (engine-core, engine-sidecar, model-client, adk-gateway,
//! repo-ingest, code-parsers, content-source, conversation) into a binary
//! with this runtime would silently
//! change every digest and every rendered object that went through
//! `serde_json::to_vec` (ADR-0029 decision 2).
//!
//! These functions write the sorted-key form whatever the feature set is. The
//! output is byte-identical to what `serde_json::to_vec`/`to_string` produce
//! WITHOUT `preserve_order`, which is how the worker has always built, so
//! moving a call site from `serde_json::to_vec(value)` to [`to_vec`] changes
//! no existing digest. Strings and numbers are still written by `serde_json`
//! itself, so escaping and the number text (including the worker's
//! `arbitrary_precision`) are unchanged.

use std::io::{self, Write};

use serde_json::Value;

/// Compact JSON of `value` with every object's members sorted by key bytes.
///
/// # Errors
///
/// Returns `serde_json`'s error when a string or number cannot be written; for
/// a [`Value`] this does not happen in practice, and callers keep their
/// existing `map_err` arms.
pub fn to_vec(value: &Value) -> Result<Vec<u8>, serde_json::Error> {
    let mut out = Vec::with_capacity(128);
    write(&mut out, value)?;
    Ok(out)
}

/// [`to_vec`] as a `String`.
///
/// # Errors
///
/// As [`to_vec`].
pub fn to_string(value: &Value) -> Result<String, serde_json::Error> {
    let bytes = to_vec(value)?;
    // serde_json only writes UTF-8; this cannot fail, but stays total.
    String::from_utf8(bytes)
        .map_err(|error| serde_json::Error::io(io::Error::new(io::ErrorKind::InvalidData, error)))
}

/// Write `value` canonically into `out`.
///
/// # Errors
///
/// As [`to_vec`], plus any error of `out`.
pub fn write<W: Write>(out: &mut W, value: &Value) -> Result<(), serde_json::Error> {
    match value {
        Value::Null | Value::Bool(_) | Value::Number(_) | Value::String(_) => {
            serde_json::to_writer(out, value)
        }
        Value::Array(items) => {
            out.write_all(b"[").map_err(serde_json::Error::io)?;
            for (index, item) in items.iter().enumerate() {
                if index > 0 {
                    out.write_all(b",").map_err(serde_json::Error::io)?;
                }
                write(out, item)?;
            }
            out.write_all(b"]").map_err(serde_json::Error::io)
        }
        Value::Object(members) => {
            let mut sorted = members.iter().collect::<Vec<_>>();
            sorted.sort_unstable_by(|left, right| left.0.as_bytes().cmp(right.0.as_bytes()));
            out.write_all(b"{").map_err(serde_json::Error::io)?;
            for (index, (key, member)) in sorted.into_iter().enumerate() {
                if index > 0 {
                    out.write_all(b",").map_err(serde_json::Error::io)?;
                }
                serde_json::to_writer(&mut *out, key)?;
                out.write_all(b":").map_err(serde_json::Error::io)?;
                write(out, member)?;
            }
            out.write_all(b"}").map_err(serde_json::Error::io)
        }
    }
}

/// Whether this build's `serde_json` keeps insertion order (`preserve_order`
/// reached it through feature unification). Diagnostics and tests only.
#[must_use]
pub fn preserve_order_linked() -> bool {
    let mut map = serde_json::Map::new();
    map.insert("b".to_owned(), Value::Null);
    map.insert("a".to_owned(), Value::Null);
    map.keys().next().is_some_and(|first| first == "b")
}

#[cfg(test)]
mod tests {
    use serde_json::{Value, json};

    use super::{to_string, to_vec};

    // Members inserted out of order. Under `preserve_order` a plain
    // `serde_json::to_vec` would keep this order; the canonical form must not.
    fn unordered() -> Value {
        serde_json::from_str(
            r#"{"zeta":{"b":[3,{"y":1,"x":"é\"q"}],"a":null},"alpha":true,"Beta":-1.5,"é":"x"}"#,
        )
        .expect("fixture")
    }

    #[test]
    fn sorts_members_by_key_bytes_at_every_depth() {
        assert_eq!(
            to_string(&unordered()).expect("encode"),
            r#"{"Beta":-1.5,"alpha":true,"zeta":{"a":null,"b":[3,{"x":"é\"q","y":1}]},"é":"x"}"#
        );
    }

    #[test]
    fn test_preserve_order_feature_reaches_serde_json() {
        if cfg!(feature = "test-preserve-order") {
            assert!(super::preserve_order_linked());
        }
    }

    #[test]
    fn equals_serde_json_without_preserve_order() {
        // Without `preserve_order` serde_json's own output is sorted, so the
        // two must agree byte for byte; with it they differ, and the
        // canonical form is the one the digests use. The libs/rust workspace
        // build links preserve_order through other members even without this
        // crate's feature, so the branch is decided by what is linked.
        let value = unordered();
        if super::preserve_order_linked() {
            assert_ne!(
                serde_json::to_vec(&value).expect("encode"),
                to_vec(&value).expect("encode"),
                "the preserve_order build must actually preserve insertion order"
            );
        } else {
            assert_eq!(
                serde_json::to_vec(&value).expect("encode"),
                to_vec(&value).expect("encode")
            );
        }
    }

    #[test]
    fn scalars_and_empty_containers_match_serde_json() {
        for value in [
            json!(null),
            json!(false),
            json!(0),
            json!(u64::MAX),
            json!(-12.25),
            json!("line\nbreak"),
            json!([]),
            json!({}),
        ] {
            assert_eq!(
                serde_json::to_vec(&value).expect("encode"),
                to_vec(&value).expect("encode")
            );
        }
    }
}
