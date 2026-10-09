//! The socket's tool table against the descriptor the Go host serves
//! (`descriptor/legacy-v1`, byte-pinned in the host): a tool cannot be served
//! and unadvertised, and an advertised tool is either served or one of the
//! deliberate exceptions below.

use elitea_inventory_engine::tools;
use serde_json::Value;
use std::collections::BTreeSet;
use std::path::PathBuf;

/// Advertised and never served by any engine: refused by the host before the
/// socket (`DeferredTools`), and — `query_graph` — routed only by
/// `inventory_search`, as the legacy router routed it.
const NOT_ROUTED: [(&str, &str); 6] = [
    ("inventory", "delta_update"),
    ("inventory", "get_type_stats"),
    ("inventory", "link_toolkits_to_tools"),
    ("inventory", "connect_orphan_nodes"),
    ("inventory", "validate_relationships"),
    ("inventory", "query_graph"),
];

#[test]
fn every_advertised_tool_is_served_or_a_named_exception() {
    let mut path = PathBuf::from(env!("CARGO_MANIFEST_DIR"));
    path.extend([
        "..",
        "..",
        "conformance",
        "provider",
        "fixtures",
        "inventory",
        "descriptor",
        "legacy-v1",
        "provider_descriptor.json",
    ]);
    let descriptor: Value = serde_json::from_str(
        &std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display())),
    )
    .unwrap_or_else(|e| panic!("{e}"));
    let mut families = 0;
    for toolkit in descriptor["provided_toolkits"]
        .as_array()
        .into_iter()
        .flatten()
    {
        let name = toolkit["name"].as_str().unwrap_or_default();
        let served: BTreeSet<&str> = tools::family(name)
            .unwrap_or_else(|| panic!("the descriptor advertises a family {name:?} nobody routes"))
            .iter()
            .copied()
            .collect();
        let advertised: BTreeSet<&str> = toolkit["provided_tools"]
            .as_array()
            .into_iter()
            .flatten()
            .filter_map(|tool| tool["name"].as_str())
            .collect();
        let excepted: BTreeSet<&str> = NOT_ROUTED
            .iter()
            .filter(|(family, _)| *family == name)
            .map(|(_, tool)| *tool)
            .collect();
        assert!(
            served.is_subset(&advertised),
            "{name}: served and not advertised: {:?}",
            served.difference(&advertised).collect::<Vec<_>>()
        );
        let expected: BTreeSet<&str> = advertised.difference(&excepted).copied().collect();
        assert_eq!(
            served, expected,
            "{name}: advertised, not served, not excepted"
        );
        families += 1;
    }
    assert_eq!(families, 2, "inventory and inventory_search");
}
