//! Adapter for converting a [`petgraph::Graph`] into a [`crate::graph::GraphData`].

use petgraph::visit::EdgeRef;

use crate::error::Result;
use crate::graph::GraphDataBuilder;

/// Convert a petgraph graph into a [`crate::graph::GraphData`].
///
/// Automatically detects whether the graph is directed or undirected and
/// constructs the appropriate CSR representation. Supports arbitrary edge
/// weight types via `E: Into<f64>`.
pub fn from_petgraph<N, E, Ty, Ix>(
    graph: &petgraph::Graph<N, E, Ty, Ix>,
) -> Result<crate::graph::GraphData>
where
    E: Into<f64> + Copy,
    Ty: petgraph::EdgeType,
    Ix: petgraph::graph::IndexType,
{
    let n = graph.node_count();
    let mut builder = GraphDataBuilder::new(n);

    if Ty::is_directed() {
        builder = builder.directed();
    }

    for edge in graph.edge_references() {
        let (u, v) = graph.edge_endpoints(edge.id()).expect("edge_endpoints should return endpoints for an existing edge during iteration");
        let weight: f64 = (*edge.weight()).into();
        builder.add_edge(u.index(), v.index(), weight)?;
    }

    builder.build()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_petgraph_empty() {
        let g: petgraph::Graph<usize, f64, petgraph::Undirected> =
            petgraph::Graph::new_undirected();
        let result = from_petgraph(&g).unwrap();
        assert_eq!(result.node_count(), 0);
        assert!(!result.is_directed());
    }

    #[test]
    fn test_petgraph_single_node() {
        let mut g = petgraph::Graph::<usize, f64, petgraph::Undirected>::new_undirected();
        g.add_node(42);
        let result = from_petgraph(&g).unwrap();
        assert_eq!(result.node_count(), 1);
        assert!(!result.is_directed());
    }

    #[test]
    fn test_petgraph_triangle_undirected() {
        let mut g = petgraph::Graph::<usize, f64, petgraph::Undirected>::new_undirected();
        let n0 = g.add_node(0);
        let n1 = g.add_node(1);
        let n2 = g.add_node(2);
        g.add_edge(n0, n1, 1.0);
        g.add_edge(n1, n2, 2.0);
        g.add_edge(n0, n2, 3.0);

        let result = from_petgraph(&g).unwrap();
        assert_eq!(result.node_count(), 3);
        assert!(!result.is_directed());
        assert!((result.total_weight() - 6.0).abs() < 1e-10);
    }

    #[test]
    fn test_petgraph_directed() {
        let mut g = petgraph::Graph::<usize, f64>::new();
        let n0 = g.add_node(0);
        let n1 = g.add_node(1);
        let n2 = g.add_node(2);
        g.add_edge(n0, n1, 1.0);
        g.add_edge(n1, n2, 2.0);
        g.add_edge(n2, n0, 3.0);

        let result = from_petgraph(&g).unwrap();
        assert_eq!(result.node_count(), 3);
        assert!(result.is_directed());
        assert!((result.total_weight() - 6.0).abs() < 1e-10);
    }

    #[test]
    fn test_petgraph_weights_preserved() {
        let mut g =
            petgraph::Graph::<usize, f64, petgraph::Undirected>::new_undirected();
        let n0 = g.add_node(0);
        let n1 = g.add_node(1);
        g.add_edge(n0, n1, 7.5);

        let result = from_petgraph(&g).unwrap();
        assert_eq!(result.node_count(), 2);
        assert!((result.total_weight() - 7.5).abs() < 1e-10);
        assert!((result.degree_of(0) - 7.5).abs() < 1e-10);
        assert!((result.degree_of(1) - 7.5).abs() < 1e-10);
    }

    #[test]
    fn test_petgraph_disconnected() {
        let mut g =
            petgraph::Graph::<usize, f64, petgraph::Undirected>::new_undirected();
        g.add_node(0);
        g.add_node(1);
        g.add_node(2);

        let result = from_petgraph(&g).unwrap();
        assert_eq!(result.node_count(), 3);
        assert!((result.total_weight() - 0.0).abs() < 1e-10);
        for i in 0..3 {
            assert!((result.degree_of(i) - 0.0).abs() < 1e-10);
        }
    }
}
