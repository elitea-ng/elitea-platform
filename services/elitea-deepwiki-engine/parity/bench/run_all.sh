#!/bin/bash
# The python-* arms need services/elitea-deepwiki, deleted after the 2026-10
# benchmark: run them from a checkout of 781bf6ece (docs/benchmark-2026-10.md).
# The benchmark's run sequence (docs/benchmark-2026-10.md): for each corpus,
# each engine variant in turn (one engine at a time against the shared model
# box): generate_wiki + the ask subset, then the retrieval question set
# against the index that run built. Expects model_router.py on :18950 and the
# dwb-pg container on :15438 (migrated).
#
#   parity/bench/run_all.sh [corpus ...]     (default: all four)
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
ENGINE_DIR=$(cd "$HERE/../.." && pwd)
SERVICES=$(cd "$ENGINE_DIR/.." && pwd)
BENCH=${BENCH:-$HOME/.cache/elitea-dw-bench}
PY=${PY:-$HOME/.cache/elitea-dw-parity/pyengine/bin/python}
BIN=${BIN:-$HOME/.cache/elitea-cargo-target-bench/release}
ROUTER=http://127.0.0.1:18950
DSN=postgres://deepwiki:deepwiki@127.0.0.1:15438/deepwiki
ENGINES=${ENGINES:-"rust python-shipped python-patched"}

repo_of() { case $1 in petclinic) echo spring-projects/spring-petclinic;; cleanarch) echo jasontaylordev/CleanArchitecture;;
  leveldb) echo google/leveldb;; express) echo expressjs/express;; esac; }
branch_of() { case $1 in express) echo master;; *) echo main;; esac; }

free_gb() { df -g /System/Volumes/Data | awk 'NR==2 {print $4}'; }

CORPORA=${*:-"petclinic cleanarch leveldb express"}
for corpus in $CORPORA; do
  for engine in $ENGINES; do
    if [ "$(free_gb)" -lt "${MIN_FREE_GB:-6}" ]; then echo "STOP: less than ${MIN_FREE_GB:-6} GB free"; exit 3; fi
    out=$BENCH/runs/$engine/$corpus
    if [ -f "$out/run.json" ] && [ -f "$BENCH/search/$engine/$corpus.jsonl" ]; then echo "skip $engine/$corpus (done)"; continue; fi
    echo "=== $engine / $corpus ($(date +%H:%M:%S))"
    rm -rf "/tmp/dwb-scratch/$engine/$corpus"
    "$PY" "$HERE/run_engine.py" "$engine" "$corpus" --repo "$(repo_of "$corpus")" --branch "$(branch_of "$corpus")" \
      --out "$out" --questions "$ENGINE_DIR/parity/questions/$corpus.jsonl" --ask 15
    mkdir -p "$BENCH/search/$engine"
    curl -s "$ROUTER/_label" -d "{\"label\": \"$engine/$corpus/search\"}" >/dev/null
    if [ "$engine" = rust ]; then
      wiki=$(podman exec dwb-pg psql -U deepwiki -d deepwiki -Atc \
        "SELECT wiki_id FROM wikis WHERE project_id = 1 AND repo = '$(repo_of "$corpus")' ORDER BY updated_at DESC NULLS LAST LIMIT 1" 2>/dev/null)
      [ -z "$wiki" ] && wiki=$(podman exec dwb-pg psql -U deepwiki -d deepwiki -Atc \
        "SELECT DISTINCT wiki_id FROM wiki_nodes WHERE project_id = 1 AND wiki_id ILIKE '%$(basename "$(repo_of "$corpus")")%' LIMIT 1")
      echo "rust index: $wiki"
      ELITEA_DEEPWIKI_DATABASE_URL=$DSN "$BIN/deepwiki-bench" search 1 "$wiki" \
        "$ENGINE_DIR/parity/questions/$corpus.jsonl" "$BENCH/search/$engine/$corpus.jsonl" \
        --api-base "$ROUTER/v1" --embedding-model Qwen/Qwen3-Embedding-4B
    else
      db=$(find "/tmp/dwb-scratch/$engine/$corpus" -name '*.wiki.db' -size +0 | head -1)
      echo "python index: $db"
      path="$SERVICES/elitea-deepwiki/src"
      [ "$engine" = python-patched ] && path="$HERE/pypatch:$path"
      DWB_EMBED_DIM=2560 PYTHONPATH="$path" "$PY" "$HERE/python_search.py" "$db" \
        "$ENGINE_DIR/parity/questions/$corpus.jsonl" "$BENCH/search/$engine/$corpus.jsonl" \
        --api-base "$ROUTER/v1" --embedding-model Qwen/Qwen3-Embedding-4B
      # Keep the index (for the record) but drop the clone.
      du -sh "/tmp/dwb-scratch/$engine/$corpus" 2>/dev/null
    fi
    curl -s "$ROUTER/_label" -d '{"label": "idle"}' >/dev/null
  done
done
