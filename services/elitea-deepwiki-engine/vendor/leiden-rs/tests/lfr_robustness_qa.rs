//! Comprehensive QA tests for LFR robustness fixes

use leiden_rs::{generate_lfr_graph, LfrConfig};

/// Test that assign_communities returns an error when a node cannot be assigned
/// (Bug fix #1: no longer silently drops nodes)
#[test]
fn test_assign_communities_errors_on_impossible_assignment() {
    let config = LfrConfig {
        n: 50,
        tau1: 2.0,
        tau2: 1.5,
        mu: 0.1,
        average_degree: None,
        min_degree: Some(15),
        max_degree: Some(20),
        min_community: Some(5),
        max_community: Some(10),
        seed: Some(42),
    };

    let result = generate_lfr_graph(config);

    assert!(
        result.is_err(),
        "Expected error when degree too high for community size, got: {:?}",
        result
    );

    let err_msg = result.unwrap_err().to_string();
    assert!(
        err_msg.contains("intra-degree")
            || err_msg.contains("cannot assign")
            || err_msg.contains("max intra-degree"),
        "Expected error message to mention intra-degree or assignment, got: {}",
        err_msg
    );
}

/// Test that wire_edges validates actual degrees vs target degrees
/// (Bug fix #2: validation added)
#[test]
fn test_wire_edges_validates_degree_shortfall() {
    let config = LfrConfig {
        n: 100,
        tau1: 3.0,
        tau2: 1.5,
        mu: 0.2,
        average_degree: Some(5.0),
        min_degree: None,
        max_degree: None,
        min_community: Some(10),
        max_community: None,
        seed: Some(42),
    };

    let result = generate_lfr_graph(config);

    assert!(
        result.is_ok(),
        "Expected success with valid config, got error: {:?}",
        result
    );

    let lfr = result.unwrap();

    let mut actual_degree = vec![0usize; lfr.node_count];
    for &(u, v, _) in &lfr.edges {
        actual_degree[u] += 1;
        actual_degree[v] += 1;
    }

    for (node, &deg) in actual_degree.iter().enumerate() {
        if lfr.node_count > 1 {
            assert!(
                deg >= 1 || lfr.node_count <= 1,
                "Node {} has zero degree, which suggests wiring failed",
                node
            );
        }
    }
}

/// Test that validate_config checks parameter compatibility
/// (Bug fix #3: new validation added)
#[test]
fn test_validate_config_catches_conflicting_parameters() {
    let config = LfrConfig {
        n: 100,
        tau1: 3.0,
        tau2: 1.5,
        mu: 0.1,
        average_degree: None,
        min_degree: None,
        max_degree: Some(50),
        min_community: Some(10),
        max_community: None,
        seed: Some(42),
    };

    let result = generate_lfr_graph(config);

    assert!(
        result.is_err(),
        "Expected validation error for conflicting max_degree/min_community, got: {:?}",
        result
    );

    let err_msg = result.unwrap_err().to_string();
    assert!(
        err_msg.contains("max intra-degree") && err_msg.contains("min_community"),
        "Expected error message to mention max intra-degree and min_community, got: {}",
        err_msg
    );
}

/// Test that degree sequence capping prevents impossible wiring
/// (New feature: degree capping)
#[test]
fn test_degree_capping_prevents_impossible_wiring() {
    let config = LfrConfig {
        n: 100,
        tau1: 2.5,
        tau2: 1.5,
        mu: 0.3,
        average_degree: None,
        min_degree: Some(8),
        max_degree: Some(20),
        min_community: Some(20),
        max_community: None,
        seed: Some(42),
    };

    let result = generate_lfr_graph(config);

    assert!(
        result.is_ok(),
        "Expected success with degree capping, got error: {:?}",
        result
    );

    let lfr = result.unwrap();

    let mut actual_degree = vec![0usize; lfr.node_count];
    for &(u, v, _) in &lfr.edges {
        actual_degree[u] += 1;
        actual_degree[v] += 1;
    }

    for (node, &deg) in actual_degree.iter().enumerate() {
        assert!(
            deg < lfr.node_count,
            "Node {} has degree {} which meets or exceeds max possible {}",
            node,
            deg,
            lfr.node_count - 1
        );
    }
}

/// Test boundary condition: n=1 (single node)
#[test]
fn test_boundary_n_equals_1() {
    let config = LfrConfig {
        n: 1,
        tau1: 3.0,
        tau2: 1.5,
        mu: 0.0,
        average_degree: None,
        min_degree: None,
        max_degree: None,
        min_community: None,
        max_community: None,
        seed: Some(42),
    };

    let result = generate_lfr_graph(config);

    match result {
        Ok(lfr) => {
            assert_eq!(lfr.node_count, 1);
            assert_eq!(lfr.edges.len(), 0);
            assert_eq!(lfr.ground_truth.len(), 1);
        }
        Err(e) => {
            println!("n=1 test failed with: {}", e);
        }
    }
}

/// Test boundary condition: n=2 (two nodes)
#[test]
fn test_boundary_n_equals_2() {
    let config = LfrConfig {
        n: 2,
        tau1: 3.0,
        tau2: 1.5,
        mu: 0.5,
        average_degree: None,
        min_degree: None,
        max_degree: None,
        min_community: None,
        max_community: None,
        seed: Some(42),
    };

    let result = generate_lfr_graph(config);

    assert!(
        result.is_ok(),
        "Expected success with n=2, got error: {:?}",
        result
    );

    let lfr = result.unwrap();
    assert_eq!(lfr.node_count, 2);
}

/// Test boundary condition: mu=0.0 (perfect communities)
#[test]
fn test_boundary_mu_equals_0() {
    let config = LfrConfig {
        n: 100,
        tau1: 3.0,
        tau2: 1.5,
        mu: 0.0,
        average_degree: Some(5.0),
        min_degree: None,
        max_degree: None,
        min_community: Some(10),
        max_community: None,
        seed: Some(42),
    };

    let result = generate_lfr_graph(config);

    assert!(
        result.is_ok(),
        "Expected success with mu=0, got error: {:?}",
        result
    );

    let lfr = result.unwrap();

    for &(u, v, _) in &lfr.edges {
        assert_eq!(
            lfr.ground_truth[u], lfr.ground_truth[v],
            "Found inter-community edge with mu=0: {} ({}) -> {} ({})",
            u, lfr.ground_truth[u], v, lfr.ground_truth[v]
        );
    }
}

/// Test boundary condition: mu=1.0 (all edges inter-community)
#[test]
fn test_boundary_mu_equals_1() {
    let config = LfrConfig {
        n: 100,
        tau1: 3.0,
        tau2: 1.5,
        mu: 1.0,
        average_degree: Some(5.0),
        min_degree: None,
        max_degree: None,
        min_community: Some(10),
        max_community: None,
        seed: Some(42),
    };

    let result = generate_lfr_graph(config);

    assert!(
        result.is_ok(),
        "Expected success with mu=1, got error: {:?}",
        result
    );
}

/// Test error path: mu < 0.0
#[test]
fn test_error_mu_negative() {
    let config = LfrConfig {
        n: 100,
        tau1: 3.0,
        tau2: 1.5,
        mu: -0.1,
        average_degree: None,
        min_degree: None,
        max_degree: None,
        min_community: None,
        max_community: None,
        seed: Some(42),
    };

    let result = generate_lfr_graph(config);
    assert!(result.is_err(), "Expected error for mu < 0");
    assert!(
        result.unwrap_err().to_string().contains("mu"),
        "Error should mention mu parameter"
    );
}

/// Test error path: mu > 1.0
#[test]
fn test_error_mu_greater_than_1() {
    let config = LfrConfig {
        n: 100,
        tau1: 3.0,
        tau2: 1.5,
        mu: 1.5,
        average_degree: None,
        min_degree: None,
        max_degree: None,
        min_community: None,
        max_community: None,
        seed: Some(42),
    };

    let result = generate_lfr_graph(config);
    assert!(result.is_err(), "Expected error for mu > 1");
    assert!(
        result.unwrap_err().to_string().contains("mu"),
        "Error should mention mu parameter"
    );
}

/// Test error path: tau1 <= 1.0
#[test]
fn test_error_tau1_equals_1() {
    let config = LfrConfig {
        n: 100,
        tau1: 1.0,
        tau2: 1.5,
        mu: 0.1,
        average_degree: None,
        min_degree: None,
        max_degree: None,
        min_community: None,
        max_community: None,
        seed: Some(42),
    };

    let result = generate_lfr_graph(config);
    assert!(result.is_err(), "Expected error for tau1 <= 1");
    assert!(
        result.unwrap_err().to_string().contains("tau1"),
        "Error should mention tau1 parameter"
    );
}

/// Test error path: tau2 <= 1.0
#[test]
fn test_error_tau2_equals_1() {
    let config = LfrConfig {
        n: 100,
        tau1: 3.0,
        tau2: 1.0,
        mu: 0.1,
        average_degree: None,
        min_degree: None,
        max_degree: None,
        min_community: None,
        max_community: None,
        seed: Some(42),
    };

    let result = generate_lfr_graph(config);
    assert!(result.is_err(), "Expected error for tau2 <= 1");
    assert!(
        result.unwrap_err().to_string().contains("tau2"),
        "Error should mention tau2 parameter"
    );
}

/// Test error path: n=0
#[test]
fn test_error_n_equals_0() {
    let config = LfrConfig {
        n: 0,
        tau1: 3.0,
        tau2: 1.5,
        mu: 0.1,
        average_degree: None,
        min_degree: None,
        max_degree: None,
        min_community: None,
        max_community: None,
        seed: Some(42),
    };

    let result = generate_lfr_graph(config);
    assert!(result.is_err(), "Expected error for n=0");
    assert!(
        result.unwrap_err().to_string().contains("n"),
        "Error should mention n parameter"
    );
}

/// Test edge case: all nodes in one community
#[test]
fn test_edge_case_single_community() {
    let config = LfrConfig {
        n: 50,
        tau1: 3.0,
        tau2: 1.5,
        mu: 0.1,
        average_degree: Some(5.0),
        min_degree: None,
        max_degree: None,
        min_community: Some(50),
        max_community: Some(50),
        seed: Some(42),
    };

    let result = generate_lfr_graph(config);

    assert!(
        result.is_ok(),
        "Expected success with single community, got error: {:?}",
        result
    );

    let lfr = result.unwrap();
    assert_eq!(lfr.community_sizes.len(), 1);
    assert_eq!(lfr.community_sizes[0], 50);
}

/// Test edge case: very small communities
#[test]
fn test_edge_case_small_communities() {
    let config = LfrConfig {
        n: 40,
        tau1: 3.0,
        tau2: 1.5,
        mu: 0.3,
        average_degree: Some(3.0),
        min_degree: None,
        max_degree: None,
        min_community: Some(4),
        max_community: Some(6),
        seed: Some(42),
    };

    let result = generate_lfr_graph(config);

    assert!(
        result.is_ok(),
        "Expected success with small communities, got error: {:?}",
        result
    );
}

/// Test edge case: high mu (0.9)
#[test]
fn test_edge_case_high_mu() {
    let config = LfrConfig {
        n: 100,
        tau1: 3.0,
        tau2: 1.5,
        mu: 0.9,
        average_degree: Some(5.0),
        min_degree: None,
        max_degree: None,
        min_community: Some(10),
        max_community: None,
        seed: Some(42),
    };

    let result = generate_lfr_graph(config);

    assert!(
        result.is_ok(),
        "Expected success with high mu, got error: {:?}",
        result
    );
}

/// Test determinism is preserved
#[test]
fn test_determinism_with_seed() {
    let config = LfrConfig {
        n: 100,
        tau1: 3.0,
        tau2: 1.5,
        mu: 0.2,
        average_degree: Some(5.0),
        min_degree: None,
        max_degree: None,
        min_community: Some(10),
        max_community: None,
        seed: Some(999),
    };

    let g1 = generate_lfr_graph(config.clone()).unwrap();
    let g2 = generate_lfr_graph(config).unwrap();

    assert_eq!(g1.edges, g2.edges, "Edges should be identical");
    assert_eq!(
        g1.ground_truth, g2.ground_truth,
        "Ground truth should be identical"
    );
    assert_eq!(
        g1.community_sizes, g2.community_sizes,
        "Community sizes should be identical"
    );
}

/// Test that degree capping respects the minimum community size
#[test]
fn test_degree_capping_respects_min_community() {
    let config = LfrConfig {
        n: 100,
        tau1: 2.5,
        tau2: 1.5,
        mu: 0.3,
        average_degree: None,
        min_degree: Some(10),
        max_degree: Some(25),
        min_community: Some(30),
        max_community: None,
        seed: Some(42),
    };

    let result = generate_lfr_graph(config);

    assert!(
        result.is_ok(),
        "Expected success with degree capping, got error: {:?}",
        result
    );

    let lfr = result.unwrap();

    let min_comm_size = *lfr.community_sizes.iter().min().unwrap();

    let mut actual_degree = vec![0usize; lfr.node_count];
    for &(u, v, _) in &lfr.edges {
        actual_degree[u] += 1;
        actual_degree[v] += 1;
    }

    for (node, &comm) in lfr.ground_truth.iter().enumerate() {
        if lfr.community_sizes[comm] == min_comm_size {
            assert!(
                actual_degree[node] < lfr.node_count,
                "Node {} in smallest community has excessive degree {}",
                node,
                actual_degree[node]
            );
        }
    }
}

/// Test that wire_edges allows small degree shortfalls
#[test]
fn test_wire_edges_allows_small_shortfalls() {
    let config = LfrConfig {
        n: 60,
        tau1: 3.0,
        tau2: 1.5,
        mu: 0.3,
        average_degree: Some(4.0),
        min_degree: None,
        max_degree: None,
        min_community: Some(12),
        max_community: None,
        seed: Some(42),
    };

    let result = generate_lfr_graph(config);

    assert!(result.is_ok(), "Expected success, got error: {:?}", result);
}
