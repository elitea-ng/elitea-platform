//! Adapter for converting a [`gryf::Graph`] into a [`crate::graph::GraphData`].

use gryf::core::{
    base::NeighborReference,
    id::{IdType, IntegerIdType},
    marker::{Directed, Direction, Undirected},
    GraphBase, GraphRef, Neighbors, VertexSet,
};

use crate::error::Result;
use crate::graph::GraphDataBuilder;

/// Convert a gryf undirected graph into a [`crate::graph::GraphData`].
pub fn from_gryf<V, E, G>(
    graph: &gryf::Graph<V, E, Undirected, G>,
) -> Result<crate::graph::GraphData>
where
    E: Into<f64> + Copy,
    G: GraphBase<VertexId: IntegerIdType, EdgeType = Undirected>
        + Neighbors
        + VertexSet
        + GraphRef<V, E>,
{
    let n = graph.vertex_count();
    let mut builder = GraphDataBuilder::new(n);

    for vertex_id in graph.vertices_by_id() {
        let u = vertex_id.as_usize();
        for neighbor in graph.neighbors_undirected(vertex_id) {
            let v = neighbor.id().as_ref().as_usize();
            if v < u {
                continue;
            }
            let edge_ref = neighbor.edge();
            let weight: f64 = graph
                .edge(edge_ref.as_ref())
                .map(|e| (*e).into())
                .unwrap_or(1.0);

            if !(weight.is_finite() && weight >= 0.0) {
                return Err(crate::error::LeidenError::InvalidEdgeWeight { weight });
            }

            builder.add_edge(u, v, weight)?;
        }
    }

    builder.build()
}

/// Convert a gryf directed graph into a [`crate::graph::GraphData`].
///
/// Iterates out-edges of each vertex so that each directed edge is added
/// exactly once to the CSR representation.
pub fn from_gryf_directed<V, E, G>(
    graph: &gryf::Graph<V, E, Directed, G>,
) -> Result<crate::graph::GraphData>
where
    E: Into<f64> + Copy,
    G: GraphBase<VertexId: IntegerIdType, EdgeType = Directed>
        + Neighbors
        + VertexSet
        + GraphRef<V, E>,
{
    let n = graph.vertex_count();
    let mut builder = GraphDataBuilder::new(n).directed();

    for vertex_id in graph.vertices_by_id() {
        let u = vertex_id.as_usize();
        for neighbor in graph.neighbors_directed(vertex_id, Direction::Outgoing) {
            let v = neighbor.id().as_ref().as_usize();
            let edge_ref = neighbor.edge();
            let weight: f64 = graph
                .edge(edge_ref.as_ref())
                .map(|e| (*e).into())
                .unwrap_or(1.0);

            if !(weight.is_finite() && weight >= 0.0) {
                return Err(crate::error::LeidenError::InvalidEdgeWeight { weight });
            }

            builder.add_edge(u, v, weight)?;
        }
    }

    builder.build()
}

#[cfg(test)]
#[cfg(feature = "gryf")]
mod tests {
    use super::*;

    #[test]
    fn test_from_gryf_triangle() {
        let mut g = gryf::Graph::new_undirected();
        let v0 = g.add_vertex(());
        let v1 = g.add_vertex(());
        let v2 = g.add_vertex(());
        g.add_edge(v0, v1, 1.0);
        g.add_edge(v1, v2, 2.0);
        g.add_edge(v0, v2, 3.0);

        let result = from_gryf(&g).unwrap();
        assert_eq!(result.node_count(), 3);
        assert!((result.total_weight() - 6.0).abs() < 1e-10);
        assert!((result.degree_of(0) - 4.0).abs() < 1e-10);
        assert!((result.degree_of(1) - 3.0).abs() < 1e-10);
        assert!((result.degree_of(2) - 5.0).abs() < 1e-10);
    }

    #[test]
    fn test_from_gryf_empty() {
        let g = gryf::Graph::<(), f64, Undirected>::new_undirected();
        let result = from_gryf(&g).unwrap();
        assert_eq!(result.node_count(), 0);
        assert!((result.total_weight() - 0.0).abs() < 1e-10);
    }

    #[test]
    fn test_from_gryf_with_weights() {
        let mut g = gryf::Graph::new_undirected();
        let v0 = g.add_vertex(());
        let v1 = g.add_vertex(());
        g.add_edge(v0, v1, 42.5);

        let result = from_gryf(&g).unwrap();
        assert_eq!(result.node_count(), 2);
        assert!((result.total_weight() - 42.5).abs() < 1e-10);

        let nbrs0: Vec<_> = result.neighbors(0).collect();
        assert_eq!(nbrs0.len(), 1);
        assert_eq!(nbrs0[0].0, 1);
        assert!((nbrs0[0].1 - 42.5).abs() < 1e-10);

        let nbrs1: Vec<_> = result.neighbors(1).collect();
        assert_eq!(nbrs1.len(), 1);
        assert_eq!(nbrs1[0].0, 0);
        assert!((nbrs1[0].1 - 42.5).abs() < 1e-10);
    }

    #[test]
    fn test_from_gryf_directed_simple() {
        let mut g = gryf::Graph::new_directed();
        let v0 = g.add_vertex(());
        let v1 = g.add_vertex(());
        g.add_edge(v0, v1, 1.0);

        let result = from_gryf_directed(&g).unwrap();
        assert_eq!(result.node_count(), 2);
        assert!(result.is_directed());

        let out: Vec<_> = result.out_neighbors(0).collect();
        assert_eq!(out.len(), 1);
        assert_eq!(out[0].0, 1);
        assert!((out[0].1 - 1.0).abs() < 1e-10);

        let out1: Vec<_> = result.out_neighbors(1).collect();
        assert_eq!(out1.len(), 0);
    }

    #[test]
    fn test_from_gryf_invalid_weight() {
        let mut g = gryf::Graph::new_undirected();
        let v0 = g.add_vertex(());
        let v1 = g.add_vertex(());
        g.add_edge(v0, v1, -1.0);

        assert!(from_gryf(&g).is_err());
    }
}
