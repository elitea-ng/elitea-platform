# End-to-end run

What this proves: the engine **executes**. A real `generate_wiki` call goes
over the sidecar protocol the Go sub-application host speaks (ADR-0023), into
the copied tool layer, out to a subprocess worker, and back with the engine's
result — clone, index, repository analysis, structure planning, page
generation. Composition into the frozen artifact set and the upload are the
host's; run the host in front of this sidecar to see them.

What it does **not** prove: content quality. `llm_stub.py` is a local,
deterministic, prompt-aware OpenAI-compatible stub. It returns a well-formed
`WikiStructureSpec` when asked for structure, names made from the listed
symbols for the cluster planner's naming prompts, and canned markdown for
pages, so the pipeline has something of the right *shape* to work with.
`LLM_STUB_RECORD=<path>` makes it append every chat request body to a file
(the Rust engine's structure parity gate compares two such records). No model is
called, nothing leaves the machine, and a run is reproducible.

It is deliberately **not** in CI: it needs the `engine` extra (~1.1 GB, torch
and friends) and a local git daemon.

## Setup

```bash
cd services/elitea-deepwiki && python -m pip install -e ".[engine,test]"
```

Serve a repository over `git://` — the engine clones with `--depth`, which the
dumb HTTP transport cannot do:

```bash
mkdir -p /tmp/dwe2e/www/acme && git clone --bare <a-small-repo> /tmp/dwe2e/www/acme/notes-service.git && git daemon --reuseaddr --export-all --base-path=/tmp/dwe2e/www --listen=127.0.0.1 --port=19418 /tmp/dwe2e/www &
```

```bash
python services/elitea-deepwiki/e2e/llm_stub.py &
```

```bash
python services/elitea-deepwiki/e2e/run_generate_wiki.py /tmp/dwe2e/scratch
```

The runner sets `ELITEA_DEEPWIKI_GIT_ALLOWLIST=127.0.0.1` for itself. The
allowlist is fail-closed — unset refuses every clone — so any other harness
has to name the hosts it clones from.

The runner rewrites `https://127.0.0.1:18900/` to `git://127.0.0.1:19418/`
through `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_0`, which is process-scoped — no
global git configuration is touched.

## What a good run looks like

An engine result with `success: true`, a `wiki_id`, a `repository_context`,
and the artifacts the host composes into the frozen set in
`conformance/fixtures/generation/composed_result.json`:

```
application/json     {wiki_id}/analysis/wiki_structure_{ts}.json
text/markdown        {wiki_id}/wiki_pages/README.md
text/markdown        {wiki_id}/wiki_pages/{section}/{page}.md
application/json     {wiki_id}/wiki_manifest_{version}.json
```

## The finding this run exists to record

**`run_in_subprocess` is not a performance switch — it changes the result.**

In-process (`run_in_subprocess=False`) the pipeline completes, but the composed
set is a *subset*: no `wiki_manifest`, and `repository_context` loses its
`{wiki_id}/` prefix. The manifest, the wiki id and the registry metadata are
built by `wiki_subprocess_worker`, not by the in-process wrapper.

So the frozen artifact set ADR-0022 decision 2 pins is only produced by the
out-of-process path. Any deployment that runs generate_wiki in-process — or any
future "simplification" that removes the subprocess hop — silently returns less
than the contract requires, and the composition fixtures alone would not catch
it, because they test the composer given a worker result rather than the worker.
