"""The canned knowledge graph :class:`~elitea_inventory.fixture_runner.FixtureToolRunner` replays.

This is Inventory's half of the two-fixture-runner trap
(``support-inventory-wiki-survey.md``, INV-2): the Go host has its OWN fixture
runner (``services/elitea-subapp-host/internal/apps/inventory/run/fixture.go``)
for the E2E stack, which has no engine image; this module is what the
standalone-full stack's engine SIDECAR answers with, over the Unix socket,
when it has no repository, no model and no real analysis engine. Nothing
forces the two to agree — they are separate languages with no shared import —
so both read the SAME checked-in graph:
``conformance/provider/fixtures/inventory/spi/graph.json``.

A wheel build does not see outside this package, so the graph is ALSO
packaged here, at ``fixtures/inventory/spi/graph.json`` — a byte-for-shape
copy. ``ELITEA_INVENTORY_FIXTURES`` overrides the packaged copy with another
directory shaped the same way (``<dir>/spi/graph.json``); the standalone-full
compose overlay uses it to point at the repository's own copy instead of
rebuilding the image. ``tests/unit/test_fixture_graph.py`` asserts the two
copies agree; ``tests/unit/test_fixture_parity.py`` is the mechanical half of
the two-runner trap: it loads every fixture file under
``conformance/provider/fixtures/inventory/{ingestion,retrieval}/`` and asserts
this module answers each one the same shape
``fixture_parity_test.go`` (Go, same files) asserts the Go runner does.

WHAT IS CANNED IS ONLY WHAT THE ENGINE WOULD HAVE COMPUTED. Composition, the
artifact hand-back and the upload are the production sidecar and host code;
only the graph data and its derived answers live here.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

# The packaged default: a checked-in copy of
# conformance/provider/fixtures/inventory/spi/graph.json. Kept in step by
# tests/unit/test_fixture_graph.py, not by a runtime read of the repository —
# a built wheel has no repository to read.
_PACKAGED_FIXTURES_DIR = Path(__file__).resolve().parent / "fixtures" / "inventory"


@dataclass(frozen=True)
class Entity:
    """One node, in the shape the legacy handlers emit."""

    id: str
    name: str
    type: str
    layer: str
    source_toolkit: str
    file_path: str
    content: str = ""

    def row(self) -> dict[str, Any]:
        """The shape every retrieval tool answers a node in — no ``content``.

        ``get_entity_content`` is the one caller that wants the body; every
        other tool answers the row below, the same fields
        ``fixtureEntityRow`` in the Go runner emits.
        """
        return {
            "id": self.id,
            "name": self.name,
            "type": self.type,
            "layer": self.layer,
            "source_toolkit": self.source_toolkit,
            "file_path": self.file_path,
        }


@dataclass(frozen=True)
class Relation:
    """One edge, ``source`` -> ``target`` under a relation type."""

    source: str
    target: str
    relation_type: str


class FixtureGraph:
    """The graph and the read-only questions the fixture tools ask of it."""

    def __init__(self, entities: list[Entity], relations: list[Relation], presets: dict[str, str]) -> None:
        self.entities = entities
        self.relations = relations
        self.presets = presets
        self._by_id = {e.id: e for e in entities}
        self._by_name = {e.name.lower(): e for e in entities}

    # -- loading -----------------------------------------------------------

    @classmethod
    def load(cls, fixtures_dir: str | Path | None = None) -> "FixtureGraph":
        """Load ``<fixtures_dir>/spi/graph.json`` (default: the packaged copy)."""
        base = Path(fixtures_dir) if fixtures_dir else _PACKAGED_FIXTURES_DIR
        path = base / "spi" / "graph.json"
        document = json.loads(path.read_text(encoding="utf-8"))
        entities = [
            Entity(
                id=row["id"], name=row["name"], type=row["type"], layer=row["layer"],
                source_toolkit=row["source_toolkit"], file_path=row["file_path"],
                content=row.get("content", ""),
            )
            for row in document["entities"]
        ]
        relations = [
            Relation(source=row["source"], target=row["target"], relation_type=row["relation_type"])
            for row in document["relations"]
        ]
        presets = dict(document.get("presets") or {})
        return cls(entities, relations, presets)

    # -- lookups -------------------------------------------------------------

    def lookup(self, ref: str) -> Entity | None:
        ref = (ref or "").strip()
        if not ref:
            return None
        if ref in self._by_id:
            return self._by_id[ref]
        return self._by_name.get(ref.lower())

    def source_of(self, entity_id: str) -> str:
        entity = self.lookup(entity_id)
        return entity.source_toolkit if entity else ""

    def source_names(self) -> list[str]:
        """Citation sources, sorted — the same rule ``fixtureSourceNames`` states."""
        return sorted({e.source_toolkit for e in self.entities})

    def match(self, query: str) -> list[Entity]:
        """Substring match over id, name and file path; empty matches everything."""
        query = (query or "").strip().lower()
        matches = []
        for entity in self.entities:
            haystack = f"{entity.id} {entity.name} {entity.file_path}".lower()
            if query == "" or query in haystack:
                matches.append(entity)
        return matches

    def count_by(self, of: Callable[[Entity], str]) -> dict[str, int]:
        counts: dict[str, int] = {}
        for entity in self.entities:
            key = of(entity)
            counts[key] = counts.get(key, 0) + 1
        return counts

    def impacted_by(self, entity_id: str) -> list[str]:
        """Everything that transitively reaches ``entity_id`` — the fixed-point closure ``fixtureImpact`` computes."""
        affected: set[str] = set()
        changed = True
        while changed:
            changed = False
            for relation in self.relations:
                if relation.target != entity_id and relation.target not in affected:
                    continue
                if relation.source not in affected:
                    affected.add(relation.source)
                    changed = True
        return sorted(affected)


def first_truthy(*values: Any) -> Any:
    for value in values:
        if value:
            return value
    return values[-1] if values else None


def entity_ref(params: dict[str, Any]) -> str:
    """Which entity a call is about, under every name the descriptor uses for it."""
    return str(first_truthy(
        params.get("entity_id"), params.get("entity_name"), params.get("entity"), params.get("id"), "",
    ))


def answer(params: dict[str, Any], document: dict[str, Any], text: str) -> dict[str, Any]:
    """The legacy ``output_format`` switch: markdown by default, JSON on request."""
    if str(params.get("output_format") or "").strip().lower() == "json":
        return {"success": True, "result": json.dumps(document)}
    return {"success": True, "result": text}


def not_found(params: dict[str, Any], what: str) -> dict[str, Any]:
    ref = str(first_truthy(
        params.get("entity_id"), params.get("entity_name"), params.get("entity"), params.get("id"),
        params.get("preset"), params.get("preset_name"), "",
    ))
    return {
        "success": False,
        "error": f"No {what} {ref!r} in this graph.",
        "error_category": "resource_not_found",
    }


def source_label_for(params: dict[str, Any]) -> str:
    """The source an ingestion ran against, ``{type}:{id}`` — mirrors ``SourceLabelFor``."""
    source = params.get("source") or {}
    if not isinstance(source, dict):
        source = {}
    kind = str(source.get("type") or "").strip()
    identifier = str(first_truthy(source.get("id"), source.get("toolkit_id"), "")).strip()
    if kind and identifier:
        return f"{kind}:{identifier}"
    if kind:
        return kind
    if identifier:
        return identifier
    return "fixture-source"


def graph_document(graph: FixtureGraph, source_label: str) -> dict[str, Any]:
    """The document ``run_ingestion`` writes as ``graph.json``."""
    nodes = [
        {
            "id": e.id, "name": e.name, "type": e.type, "layer": e.layer,
            "file_path": e.file_path,
            "citations": [{"source_toolkit": e.source_toolkit, "file_path": e.file_path}],
        }
        for e in graph.entities
    ]
    edges = [
        {"source": r.source, "target": r.target, "relation_type": r.relation_type}
        for r in graph.relations
    ]
    return {
        "nodes": nodes,
        "edges": edges,
        "_metadata": {
            "fixture": True,
            "schema_version": 1,
            "source_toolkits": graph.source_names(),
            "ingested_source": source_label,
            "node_count": len(graph.entities),
            "edge_count": len(graph.relations),
        },
    }


# ---------------------------------------------------------------------------
# the answers — one function per tool, mirroring fixture.go's fixtureHandlers
# ---------------------------------------------------------------------------


def run_ingestion(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    label = source_label_for(params)
    doc = graph_document(graph, label)
    status = {
        "sources": [
            {
                "source": label, "status": "completed",
                "entity_count": len(graph.entities), "relation_count": len(graph.relations),
            }
        ]
    }
    checkpoint = {"source": label, "stage": "completed", "files_processed": 12}
    return {
        "success": True,
        "result": (
            f"Ingestion completed for {label}: {len(graph.entities)} entities, "
            f"{len(graph.relations)} relations from 12 files."
        ),
        "artifacts": [
            {"name": "graph.json", "type": "application/json", "data": json.dumps(doc)},
            {"name": "sources_status.json", "type": "application/json", "data": json.dumps(status)},
            {
                "name": f".ingestion-checkpoint-{label}.json",
                "type": "application/json", "data": json.dumps(checkpoint),
            },
        ],
    }


def remove_source_entities(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    label = str(first_truthy(
        params.get("toolkit_id"), params.get("source_toolkit"), params.get("source"), "",
    )).strip() or source_label_for(params)
    removed = sum(1 for e in graph.entities if e.source_toolkit == label)
    return answer(params, {"source": label, "removed_entities": removed},
                  f"Removed {removed} entities contributed by {label}.")


def list_ingested_sources(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    names = graph.source_names()
    rows = []
    lines = [f"# Ingested Sources ({len(names)})", ""]
    for name in names:
        entities = sum(1 for e in graph.entities if e.source_toolkit == name)
        relations = sum(1 for r in graph.relations if graph.source_of(r.source) == name)
        rows.append({"source_toolkit": name, "entity_count": entities, "relation_count": relations})
        lines.append(f"- **{name}**: {entities} entities, {relations} relations")
    return answer(params, {"sources": rows, "total_sources": len(names)}, "\n".join(lines) + "\n")


def get_sources_status(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    names = graph.source_names()
    rows = []
    lines = ["# Sources", ""]
    for name in names:
        entities = sum(1 for e in graph.entities if e.source_toolkit == name)
        rows.append({"source": name, "status": "completed", "entity_count": entities})
        lines.append(f"- **{name}**: completed, {entities} entities")
    return answer(params, {"sources": rows}, "\n".join(lines) + "\n")


def get_ingestion_status(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    return answer(
        params,
        {
            "running": False, "last_status": "completed",
            "entity_count": len(graph.entities), "relation_count": len(graph.relations),
        },
        "No ingestion is running. The last one completed.",
    )


def list_graphs(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    name = _graph_name(params)
    bucket = _bucket(params)
    return answer(params, {"graphs": [{"name": name, "size": 4096}], "bucket": bucket},
                  f"# Available Graphs in '{bucket}'\n\n- **{name}** (4.0 KB)\n")


def load_graph(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    name = _graph_name(params)
    return answer(
        params,
        {"graph_name": name, "node_count": len(graph.entities), "edge_count": len(graph.relations)},
        f"Loaded graph: {name}\nNodes: {len(graph.entities)}, Edges: {len(graph.relations)}",
    )


def get_graph_info(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    name = _graph_name(params)
    sources = graph.source_names()
    return answer(
        params,
        {
            "path": f"{_bucket(params)}/{name}/graph.json",
            "node_count": len(graph.entities), "edge_count": len(graph.relations),
            "source_toolkits": sources,
        },
        f"# Graph {name}\n\nNodes: {len(graph.entities)}\nEdges: {len(graph.relations)}\n"
        f"Sources: {', '.join(sources)}\n",
    )


def search_graph(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    query = str(first_truthy(params.get("query"), params.get("search_query"), params.get("question"), "")).strip().lower()
    matches = graph.match(query)
    rows = [e.row() for e in matches]
    lines = [f"# Results for {query!r} ({len(matches)})", ""]
    for e in matches:
        lines.append(f"- **{e.name}** ({e.type}, {e.layer}) — {e.file_path}")
    return answer(params, {"query": query, "results": rows, "total": len(matches)}, "\n".join(lines) + "\n")


def get_entity(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    entity = graph.lookup(entity_ref(params))
    if entity is None:
        return not_found(params, "entity")
    return answer(
        params, entity.row(),
        f"# {entity.name}\n\n- id: {entity.id}\n- type: {entity.type}\n- layer: {entity.layer}\n"
        f"- source: {entity.source_toolkit}\n- file: {entity.file_path}\n",
    )


def get_entity_content(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    entity = graph.lookup(entity_ref(params))
    if entity is None:
        return not_found(params, "entity")
    return answer(
        params,
        {"id": entity.id, "file_path": entity.file_path, "content": entity.content},
        f"```\n{entity.content}```\n",
    )


def get_entities_by_ids(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    raw_ids = params.get("entity_ids") or params.get("ids") or []
    rows, missing = [], []
    for raw in raw_ids:
        entity_id = str(raw).strip()
        entity = graph.lookup(entity_id)
        if entity is not None:
            rows.append(entity.row())
        else:
            missing.append(entity_id)
    return answer(
        params, {"entities": rows, "missing": missing},
        f"Found {len(rows)} of {len(rows) + len(missing)} entities.",
    )


def get_related_entities(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    entity_id = entity_ref(params)
    if graph.lookup(entity_id) is None:
        return not_found(params, "entity")
    rows = []
    lines = [f"# Neighbours of {entity_id}", ""]
    for relation in graph.relations:
        if relation.source == entity_id:
            rows.append({"entity_id": relation.target, "relation_type": relation.relation_type, "direction": "outgoing"})
            lines.append(f"- {entity_id} → {relation.target} ({relation.relation_type})")
        elif relation.target == entity_id:
            rows.append({"entity_id": relation.source, "relation_type": relation.relation_type, "direction": "incoming"})
            lines.append(f"- {entity_id} ← {relation.source} ({relation.relation_type})")
    return answer(params, {"entity_id": entity_id, "related": rows, "total": len(rows)}, "\n".join(lines) + "\n")


def impact_analysis(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    entity_id = entity_ref(params)
    if graph.lookup(entity_id) is None:
        return not_found(params, "entity")
    impacted = graph.impacted_by(entity_id)
    return answer(
        params, {"entity_id": entity_id, "impacted": impacted, "total": len(impacted)},
        f"# Impact of {entity_id}\n\n{len(impacted)} entities depend on it: {', '.join(impacted)}\n",
    )


def get_cross_source_relations(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    rows = []
    lines = ["# Cross-source relations", ""]
    for relation in graph.relations:
        from_source, to_source = graph.source_of(relation.source), graph.source_of(relation.target)
        if from_source == to_source or not from_source or not to_source:
            continue
        rows.append({
            "source": relation.source, "target": relation.target, "relation_type": relation.relation_type,
            "from_source": from_source, "to_source": to_source,
        })
        lines.append(f"- {relation.source} ({from_source}) → {relation.target} ({to_source}) [{relation.relation_type}]")
    return answer(params, {"relations": rows, "total": len(rows)}, "\n".join(lines) + "\n")


def get_stats(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    by_type = graph.count_by(lambda e: e.type)
    by_layer = graph.count_by(lambda e: e.layer)
    return answer(
        params,
        {
            "node_count": len(graph.entities), "edge_count": len(graph.relations),
            "entities_by_type": by_type, "entities_by_layer": by_layer,
            "source_toolkits": graph.source_names(),
        },
        f"# Graph statistics\n\nNodes: {len(graph.entities)}\nEdges: {len(graph.relations)}\n"
        f"Types: {len(by_type)}\nLayers: {len(by_layer)}\n",
    )


def _filtered(graph: FixtureGraph, params: dict[str, Any], key: str, of: Callable[[Entity], str]) -> dict[str, Any]:
    wanted = str(first_truthy(params.get(key), params.get("type"), params.get("value"), "")).strip()
    rows = []
    lines = [f"# Entities where {key} = {wanted!r}", ""]
    for entity in graph.entities:
        if wanted and of(entity).lower() != wanted.lower():
            continue
        rows.append(entity.row())
        lines.append(f"- **{entity.name}** ({of(entity)}) — {entity.file_path}")
    return answer(params, {key: wanted, "entities": rows, "total": len(rows)}, "\n".join(lines) + "\n")


def list_entities_by_type(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    return _filtered(graph, params, "entity_type", lambda e: e.type)


def list_entities_by_layer(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    return _filtered(graph, params, "layer", lambda e: e.layer)


def list_entities_by_source(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    return _filtered(graph, params, "source_toolkit", lambda e: e.source_toolkit)


def list_entity_types(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    counts = graph.count_by(lambda e: e.type)
    names = sorted(counts)
    lines = ["# Entity types", ""]
    for name in names:
        lines.append(f"- **{name}**: {counts[name]}")
    return answer(params, {"types": names, "counts": counts}, "\n".join(lines) + "\n")


def query_graph(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    query = str(first_truthy(params.get("query"), params.get("question"), "")).strip()
    matches = graph.match(query.lower())
    rows = [e.row() for e in matches]
    return answer(
        params, {"query": query, "matches": rows, "total": len(matches)},
        f"# Query: {query}\n\n{len(matches)} matching entities.\n",
    )


def investigate(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    question = str(first_truthy(params.get("question"), params.get("query"), "")).strip()
    matches = graph.match(question.lower())
    cited = [e.id for e in matches]
    lines = [f"- {e.name} ({e.file_path})" for e in matches]
    text = f"[fixture] Answer to {question!r}, from the canned graph:\n\n" + "\n".join(lines) + ("\n" if lines else "")
    return answer(params, {"question": question, "answer": text, "entities": cited}, text)


def list_presets(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    names = sorted(graph.presets)
    lines = ["# Presets", ""]
    for name in names:
        lines.append(f"- **{name}**: {graph.presets[name]}")
    return answer(params, {"presets": names}, "\n".join(lines) + "\n")


def get_preset_info(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    name = str(first_truthy(params.get("preset"), params.get("preset_name"), "")).strip()
    description = graph.presets.get(name)
    if description is None:
        return not_found(params, "preset")
    return answer(params, {"preset": name, "description": description}, f"# {name}\n\n{description}\n")


def get_cache_stats(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    return answer(params, {"cached_graphs": 1, "cache_size_bytes": 4096, "hits": 0, "misses": 1},
                  "# Cache\n\nGraphs: 1\nSize: 4.0 KB\n")


def cleanup_cache(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    return answer(params, {"removed_graphs": 1, "freed_bytes": 4096}, "Removed 1 cached graph (4.0 KB).")


def normalize_types(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    counts = graph.count_by(lambda e: e.type)
    return answer(params, {"normalized": 0, "types": counts},
                  f"Types are already normalised: {len(counts)} distinct types.")


def rebuild_indices(graph: FixtureGraph, params: dict[str, Any]) -> dict[str, Any]:
    return answer(params, {"indexed_entities": len(graph.entities)},
                  f"Rebuilt the indices over {len(graph.entities)} entities.")


def _graph_name(params: dict[str, Any]) -> str:
    name = str(first_truthy(params.get("graph_name"), params.get("toolkit_configuration_graph_name"), "")).strip()
    return name or "inventory"


def _bucket(params: dict[str, Any]) -> str:
    return str(params.get("bucket") or "inventory")


#: The canned tool table: one entry per name the descriptor advertises, the
#: SAME keys ``fixtureHandlers()`` in the Go runner carries — aliases included.
FIXTURE_HANDLERS: dict[str, Callable[[FixtureGraph, dict[str, Any]], dict[str, Any]]] = {
    # ── ingestion ──
    "run_ingestion": run_ingestion,
    "remove_source_entities": remove_source_entities,
    # ── graph management ──
    "list_ingested_sources": list_ingested_sources,
    "list_graphs": list_graphs,
    "load_graph": load_graph,
    "get_graph_info": get_graph_info,
    # ── retrieval ──
    "search_graph": search_graph,
    "search_knowledge_graph": search_graph,
    "get_entity": get_entity,
    "get_entity_details": get_entity,
    "get_entity_content": get_entity_content,
    "get_entities_by_ids": get_entities_by_ids,
    "get_related_entities": get_related_entities,
    "get_entity_neighbors": get_related_entities,
    "impact_analysis": impact_analysis,
    "get_cross_source_relations": get_cross_source_relations,
    "get_stats": get_stats,
    "list_entities_by_type": list_entities_by_type,
    "list_entities_by_layer": list_entities_by_layer,
    "list_entities_by_source": list_entities_by_source,
    "list_entity_types": list_entity_types,
    "query_graph": query_graph,
    "investigate": investigate,
    # ── presets ──
    "list_presets": list_presets,
    "get_preset_info": get_preset_info,
    # ── cache ──
    "get_cache_stats": get_cache_stats,
    "cleanup_cache": cleanup_cache,
    # ── status ──
    "get_ingestion_status": get_ingestion_status,
    "get_sources_status": get_sources_status,
    # ── maintenance ──
    "normalize_types": normalize_types,
    "smart_normalize_types": normalize_types,
    "rebuild_indices": rebuild_indices,
}


__all__ = [
    "Entity", "Relation", "FixtureGraph", "FIXTURE_HANDLERS",
    "answer", "not_found", "entity_ref", "source_label_for", "graph_document",
]
