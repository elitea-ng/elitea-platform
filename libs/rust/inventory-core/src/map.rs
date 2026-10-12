//! `serde_json::Map` operations whose result does not depend on whether
//! `serde_json` is built with `preserve_order`.
//!
//! `Map::shift_remove` exists only with the feature, and `Map::remove` is
//! `swap_remove` with it (the last key takes the removed one's place). The
//! engine links the feature and the desktop must not (ADR-0029 decision 2),
//! so this crate removes keys through [`shift_remove`], which keeps the order
//! of the other keys in both builds: exactly `Map::shift_remove` with the
//! feature, and the sorted map's `remove` without it.

use serde_json::{Map, Value};

/// Remove `key` from `map`, keeping the other keys in their order.
pub fn shift_remove(map: &mut Map<String, Value>, key: &str) -> Option<Value> {
    if !map.contains_key(key) {
        return None;
    }
    let mut removed = None;
    map.retain(|candidate, value| {
        if candidate == key {
            removed = Some(std::mem::take(value));
            false
        } else {
            true
        }
    });
    removed
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn map_of(keys: &[&str]) -> Map<String, Value> {
        keys.iter()
            .map(|key| ((*key).to_owned(), json!(key)))
            .collect()
    }

    #[test]
    fn the_other_keys_keep_their_order() {
        let mut map = map_of(&["z", "y", "x", "w"]);
        let before: Vec<String> = map.keys().cloned().collect();
        assert_eq!(shift_remove(&mut map, "y"), Some(json!("y")));
        assert_eq!(shift_remove(&mut map, "missing"), None);
        let after: Vec<String> = map.keys().cloned().collect();
        let expected: Vec<String> = before.into_iter().filter(|key| key != "y").collect();
        // Insertion order with `preserve_order` (where `remove` would have
        // moved "w" into "y"'s place), sorted order without.
        assert_eq!(after, expected);
    }
}
