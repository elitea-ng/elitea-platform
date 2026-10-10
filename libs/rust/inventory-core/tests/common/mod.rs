//! What the golden tests share: comparing an answer with its golden in
//! either `serde_json` build.

#![allow(dead_code)]

use serde_json::{Map, Value};
use std::collections::BTreeSet;

/// Whether this build's `serde_json` keeps a map's keys in insertion order
/// (`preserve_order`: the engine, and the libs/rust workspace, where other
/// members turn it on) or sorted (this crate built alone, as the desktop
/// host links it).
pub fn preserves_order() -> bool {
    let map: Map<String, Value> = serde_json::from_str(r#"{"b": 0, "a": 0}"#).unwrap();
    map.keys().next().is_some_and(|key| key == "b")
}

/// A JSON value with every object's keys sorted, arrays kept in order.
pub fn sorted_keys(value: &Value) -> Value {
    match value {
        Value::Object(fields) => {
            let mut keys: Vec<&String> = fields.keys().collect();
            keys.sort();
            let mut sorted = Map::new();
            for key in keys {
                sorted.insert(key.clone(), sorted_keys(&fields[key]));
            }
            Value::Object(sorted)
        }
        Value::Array(items) => Value::Array(items.iter().map(sorted_keys).collect()),
        other => other.clone(),
    }
}

/// An answer with map key order taken out: a JSON document (the whole
/// answer, or one line of it) with its keys sorted, and each run of
/// `key: value` attribute lines (`- key: value`, `  key: value`) sorted.
pub fn order_free(text: &str) -> String {
    if let Ok(document) = serde_json::from_str::<Value>(text)
        && (document.is_object() || document.is_array())
    {
        return sorted_keys(&document).to_string();
    }
    let attribute = |line: &str| {
        let body = line.trim_start().trim_start_matches("- ");
        body.split_once(": ")
            .is_some_and(|(key, _)| !key.is_empty() && !key.contains(' ') && !key.starts_with('#'))
    };
    let mut out: Vec<String> = Vec::new();
    let mut run: Vec<String> = Vec::new();
    for line in text.split('\n') {
        let line = match serde_json::from_str::<Value>(line.trim()) {
            Ok(document) if document.is_object() || document.is_array() => {
                sorted_keys(&document).to_string()
            }
            _ => line.to_owned(),
        };
        if attribute(&line) {
            run.push(line);
        } else {
            run.sort();
            out.append(&mut run);
            out.push(line);
        }
    }
    run.sort();
    out.append(&mut run);
    out.join("\n")
}

/// The answers a golden test compared, and which of them differed from
/// their golden only by map key order (possible only built alone).
#[derive(Debug)]
pub struct Goldens {
    ordered: bool,
    order_dependent: BTreeSet<String>,
}

/// The variable CI sets to the `serde_json` build a run must be in
/// (`preserve` or `sorted`): the two runs check different things, and a
/// run in the other build would pass while checking the wrong one.
pub const EXPECTED_ORDER_ENV: &str = "INVENTORY_CORE_SERDE_ORDER";

impl Default for Goldens {
    fn default() -> Self {
        let ordered = preserves_order();
        match std::env::var(EXPECTED_ORDER_ENV).as_deref() {
            Ok("preserve") => assert!(
                ordered,
                "{EXPECTED_ORDER_ENV}=preserve, but maps are sorted"
            ),
            Ok("sorted") => assert!(
                !ordered,
                "{EXPECTED_ORDER_ENV}=sorted, but maps keep insertion order"
            ),
            Ok(other) => panic!("{EXPECTED_ORDER_ENV} is `preserve` or `sorted`, not {other:?}"),
            Err(_) => {}
        }
        Self {
            ordered,
            order_dependent: BTreeSet::new(),
        }
    }
}

impl Goldens {
    /// Compare `got` with `want` for the handler `handler`: byte for byte
    /// with `preserve_order`; without it, either byte for byte or, when the
    /// handler's text lists a map's keys, equal but for their order.
    ///
    /// # Panics
    ///
    /// When they differ (beyond key order, built alone).
    pub fn check(&mut self, handler: &str, got: &str, want: &str, label: &str) {
        if self.ordered {
            assert_eq!(got, want, "{label}");
        } else if got != want {
            if std::env::var_os("SHOW_ORDER_DIFF").is_some() {
                for (g, w) in got.lines().zip(want.lines()) {
                    if g != w {
                        eprintln!("{label}\n  got:  {g}\n  want: {w}");
                    }
                }
            }
            assert_eq!(
                order_free(got),
                order_free(want),
                "{label}: differs beyond map key order"
            );
            self.order_dependent.insert(handler.to_owned());
        }
    }

    /// Check the handlers found to depend on key order against `listed`:
    /// none with `preserve_order`, exactly `listed` without it.
    ///
    /// # Panics
    ///
    /// When they disagree.
    pub fn finish(&self, listed: &[&str]) {
        if self.ordered {
            assert!(self.order_dependent.is_empty());
        } else {
            let listed: BTreeSet<String> = listed.iter().map(|s| (*s).to_owned()).collect();
            assert_eq!(
                self.order_dependent, listed,
                "the handlers whose text depends on map key order changed: update the list"
            );
        }
    }
}
