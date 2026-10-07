#!/usr/bin/env python3
"""
Verify leiden-rs test expectations against leidenalg.

This script runs leidenalg (the reference Leiden implementation by V.A. Traag)
and checks that the hardcoded expected values in the Rust integration tests
match leidenalg's output. Run with pytest:

    pytest scripts/verify_leidenalg.py -v

If this test fails, the hardcoded values in tests/verify_correctness.rs have
been manually edited or leidenalg has changed — investigate before proceeding.
"""

import pytest
import igraph as ig
import leidenalg as la


def normalize_membership(membership):
    if not membership:
        return []
    n = len(membership)
    communities = {}
    for node, comm in enumerate(membership):
        if comm not in communities:
            communities[comm] = []
        communities[comm].append(node)
    sorted_comms = sorted(communities.values(), key=lambda nodes: min(nodes))
    normalized = [0] * n
    for label, nodes in enumerate(sorted_comms):
        for node in nodes:
            normalized[node] = label
    return normalized


def make_two_cliques():
    g = ig.Graph(n=10)
    for i in range(5):
        for j in range(i + 1, 5):
            g.add_edge(i, j, weight=1.0)
    for i in range(5, 10):
        for j in range(i + 1, 10):
            g.add_edge(i, j, weight=1.0)
    g.add_edge(0, 5, weight=1.0)
    return g


def make_ring_of_cliques():
    g = ig.Graph(n=12)
    for base in [0, 4, 8]:
        for i in range(base, base + 4):
            for j in range(i + 1, base + 4):
                g.add_edge(i, j, weight=1.0)
    g.add_edge(0, 4, weight=0.1)
    g.add_edge(4, 8, weight=0.1)
    g.add_edge(8, 0, weight=0.1)
    return g


def make_weighted_graph():
    g = ig.Graph(n=6)
    for i in range(3):
        for j in range(i + 1, 3):
            g.add_edge(i, j, weight=5.0)
    for i in range(3, 6):
        for j in range(i + 1, 6):
            g.add_edge(i, j, weight=5.0)
    g.add_edge(0, 3, weight=0.1)
    return g


def make_disconnected_graph():
    g = ig.Graph(n=6)
    g.add_edge(0, 1, weight=1.0)
    g.add_edge(0, 2, weight=1.0)
    g.add_edge(1, 2, weight=1.0)
    g.add_edge(3, 4, weight=1.0)
    g.add_edge(3, 5, weight=1.0)
    g.add_edge(4, 5, weight=1.0)
    return g


def make_zachary():
    return ig.Graph.Famous("Zachary")


def make_large_clustered():
    import random
    random.seed(42)
    g = ig.Graph(n=100)
    clusters = [list(range(c * 25, (c + 1) * 25)) for c in range(4)]
    for cluster in clusters:
        for i in range(len(cluster)):
            for j in range(i + 1, len(cluster)):
                if random.random() < 0.3:
                    g.add_edge(cluster[i], cluster[j], weight=1.0)
    for i in range(4):
        for j in range(i + 1, 4):
            pairs = [(a, b) for a in clusters[i] for b in clusters[j]]
            for a, b in random.sample(pairs, min(3, len(pairs))):
                g.add_edge(a, b, weight=0.1)
    return g


class TestModularityGroundTruth:
    """Verify that hardcoded Modularity expected values match leidenalg."""

    def test_two_cliques(self):
        g = make_two_cliques()
        part = la.find_partition(g, la.ModularityVertexPartition, seed=42)
        assert part.quality() == pytest.approx(0.4523809524, abs=1e-4)
        assert len(part) == 2
        assert normalize_membership(list(part.membership)) == [
            0, 0, 0, 0, 0, 1, 1, 1, 1, 1
        ]

    def test_ring_of_cliques(self):
        g = make_ring_of_cliques()
        part = la.find_partition(g, la.ModularityVertexPartition, seed=42)
        assert part.quality() == pytest.approx(0.5238095238, abs=1e-4)
        assert len(part) == 3

    def test_weighted_graph(self):
        g = make_weighted_graph()
        part = la.find_partition(g, la.ModularityVertexPartition, seed=42)
        assert part.quality() == pytest.approx(0.3571428571, abs=1e-4)
        assert len(part) == 2
        assert normalize_membership(list(part.membership)) == [
            0, 0, 0, 1, 1, 1
        ]

    def test_disconnected(self):
        g = make_disconnected_graph()
        part = la.find_partition(g, la.ModularityVertexPartition, seed=42)
        assert part.quality() == pytest.approx(0.5, abs=1e-4)
        assert len(part) == 2
        assert normalize_membership(list(part.membership)) == [
            0, 0, 0, 1, 1, 1
        ]

    def test_zachary(self):
        g = make_zachary()
        part = la.find_partition(g, la.ModularityVertexPartition, seed=42)
        assert part.quality() == pytest.approx(0.4197896121, abs=1e-4)

    def test_large_clustered(self):
        g = make_large_clustered()
        part = la.find_partition(g, la.ModularityVertexPartition, seed=42)
        assert part.quality() == pytest.approx(0.7002596752, abs=1e-4)

    def test_self_loop(self):
        g = ig.Graph(n=4)
        g.add_edge(0, 1, weight=1.0)
        g.add_edge(2, 3, weight=1.0)
        g.add_edge(0, 0, weight=2.0)
        # igraph standard modularity for this partition
        assert g.modularity([0, 0, 1, 1], weights="weight") == pytest.approx(
            0.375, abs=1e-4
        )


class TestCPMGroundTruth:
    """Verify CPM expected values match leidenalg (halved for our scale)."""

    def test_two_cliques_low_resolution(self):
        g = make_two_cliques()
        part = la.find_partition(
            g, la.CPMVertexPartition, resolution_parameter=0.1, seed=42
        )
        assert len(part) == 2
        # leidenalg quality is 2x our scale
        assert part.quality() / 2.0 == pytest.approx(18.0, abs=0.1)

    def test_two_cliques_high_resolution(self):
        g = make_two_cliques()
        part = la.find_partition(
            g, la.CPMVertexPartition, resolution_parameter=1.0, seed=42
        )
        assert len(part) >= 8

    def test_large_clustered(self):
        g = make_large_clustered()
        part = la.find_partition(
            g, la.CPMVertexPartition, resolution_parameter=0.05, seed=42
        )
        assert 3 <= len(part) <= 5
        assert part.quality() / 2.0 == pytest.approx(301.0, abs=1.0)

    def test_disconnected_low_resolution(self):
        g = make_disconnected_graph()
        part = la.find_partition(
            g, la.CPMVertexPartition, resolution_parameter=0.1, seed=42
        )
        assert len(part) == 2
        # leidenalg quality = 10.8, our scale = 5.4
        assert part.quality() / 2.0 == pytest.approx(5.4, abs=0.1)

    def test_self_loop_low_resolution(self):
        g = ig.Graph(n=4)
        g.add_edge(0, 1, weight=1.0)
        g.add_edge(2, 3, weight=1.0)
        g.add_edge(0, 0, weight=2.0)
        part = la.find_partition(
            g, la.CPMVertexPartition, resolution_parameter=0.1, seed=42
        )
        assert len(part) == 2

    def test_self_loop_high_resolution(self):
        g = ig.Graph(n=4)
        g.add_edge(0, 1, weight=1.0)
        g.add_edge(2, 3, weight=1.0)
        g.add_edge(0, 0, weight=2.0)
        part = la.find_partition(
            g, la.CPMVertexPartition, resolution_parameter=1.0, seed=42
        )
        assert len(part) >= 3


class TestMultiSeedConsistency:
    """Verify leidenalg produces consistent structure across seeds."""

    @pytest.mark.parametrize("seed", [0, 1, 42, 123, 999])
    def test_two_cliques_always_2_communities(self, seed):
        g = make_two_cliques()
        part = la.find_partition(g, la.ModularityVertexPartition, seed=seed)
        assert len(part) == 2
        assert part.quality() > 0.4

    @pytest.mark.parametrize("seed", [0, 1, 42, 123, 999])
    def test_zachary_reasonable(self, seed):
        g = make_zachary()
        part = la.find_partition(g, la.ModularityVertexPartition, seed=seed)
        assert 2 <= len(part) <= 5
        assert part.quality() > 0.35

    @pytest.mark.parametrize("seed", [0, 1, 42, 123, 999])
    def test_large_clustered_reasonable(self, seed):
        g = make_large_clustered()
        part = la.find_partition(g, la.ModularityVertexPartition, seed=seed)
        assert len(part) >= 3
        assert part.quality() > 0.5
