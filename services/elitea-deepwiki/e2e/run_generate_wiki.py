"""End-to-end: drive the engine SIDECAR, engine wired, against a real repository.

Proves the engine EXECUTES — clone, index, repository analysis, structure
planning, page generation — over the protocol the Go sub-application host
speaks to it (ADR-0023): one NDJSON stream of progress, then the engine's
result. Composition into the frozen object set and the upload are the host's
now, so what this prints is the ENGINE result (artifacts by name and size).
It is not a content test: the LLM is a local stub
(``services/elitea-deepwiki-engine/testdata/llm_stub.py``) whose
answers are canned and deterministic. What is under test is the pipeline,
not the prose.

Not wired into CI: it needs the ~1.1 GB engine extra and a local git daemon.
See e2e/README.md for the setup.

    python e2e/run_generate_wiki.py <scratch-dir>

Environment:
    E2E_TIMEOUT   seconds to wait for the invocation (default 600)
    E2E_DUMP      path to write the result objects as JSON
"""
import asyncio, json, os, pathlib, sys, warnings
warnings.filterwarnings("ignore")

os.environ.setdefault("ELITEA_DEEPWIKI_RUNNER", "legacy")
os.environ.setdefault("ELITEA_DEEPWIKI_SCRATCH_PATH", sys.argv[1])
# The egress allowlist is FAIL-CLOSED: unset refuses every clone. The harness
# clones from the local git daemon, so that host is what it permits.
os.environ.setdefault("ELITEA_DEEPWIKI_GIT_ALLOWLIST", "127.0.0.1")

os.environ["GIT_CONFIG_COUNT"] = "1"
# git daemon, not dumb HTTP: the engine clones with --depth, and dumb HTTP
# transport does not support shallow capabilities. insteadOf can rewrite the
# scheme as well as the host, and this is process-scoped — no global git config
# is touched.
os.environ["GIT_CONFIG_KEY_0"] = "url.git://127.0.0.1:19418/.insteadOf"
os.environ["GIT_CONFIG_VALUE_0"] = "https://127.0.0.1:18900/"

from httpx import ASGITransport, AsyncClient
from elitea_deepwiki.config import Settings
from elitea_deepwiki.sidecar import create_sidecar

MOCK = "http://127.0.0.1:18901/v1"

REQUEST = {
    "configuration": {"parameters": {
        # repository/active_branch sit at the TOOLKIT level, not inside
        # github_configuration: _extract_repo_config_from_toolkit reads them
        # off the merged toolkit payload. Putting them only in the provider
        # block yields repo_config["repository"] = None and the engine refuses
        # with "GitHub repository not specified".
        "code_toolkit": {
            # GitHub reads base_url; GitLab reads url. Not interchangeable.
            "github_configuration": {"base_url": "http://127.0.0.1:18900"},
            "repository": "acme/notes-service",
            "active_branch": "main",
        },
        "llm_settings": {
            # Both spellings on purpose: the in-process path requires
            # openai_api_*, the subprocess worker accepts either.
            "model_name": "gpt-4o",
            "api_base": MOCK, "api_key": "mock",
            "openai_api_base": MOCK, "openai_api_key": "mock",
            "organization": "1", "max_tokens": 4000,
        },
        # A STRING, despite the descriptor declaring this parameter as JSON:
        # the engine passes it straight to OpenAIEmbeddings(model=...).
        "embedding_model": "text-embedding-3-small",
    }},
    "parameters": {"query": "Document the notes service", "planner_type": "cluster",
        "run_in_subprocess": True},
}


def engine_arguments(request: dict) -> dict:
    """The legacy keyword set the Go host derives (ArgumentsFor) for generate_wiki."""
    params = dict(request["configuration"]["parameters"])
    params.update(request["parameters"])
    toolkit = params["code_toolkit"]
    repo_config = {
        "provider_type": "github",
        "provider_config": toolkit["github_configuration"],
        "repository": toolkit.get("repository"),
        "branch": toolkit.get("active_branch", "main"),
        "project": None,
        "is_cloud": None,
    }
    return {
        "llm_settings": params.get("llm_settings") or {},
        "embedding_model": params.get("embedding_model"),
        "query": params["query"],
        "repo_config": repo_config,
        "active_branch": toolkit.get("active_branch", "main"),
        "force_rebuild_index": params.get("force_rebuild_index", True),
        "indexing_method": params.get("indexing_method", "filesystem"),
        "planner_mode": params.get("planner_mode") or params.get("planner_type"),
        "exclude_tests": params.get("exclude_tests"),
        "run_in_subprocess": params.get("run_in_subprocess", True),
    }


async def main():
    settings = Settings.from_env()
    app = create_sidecar(settings)
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://engine", timeout=None) as http:
        health = (await http.get("/engine/health")).json()
        print("runner:", health["runner"])
        body = {"invocation_id": "invocation_e2e", "tool": "generate_wiki", "arguments": engine_arguments(REQUEST)}
        # httpx's ASGI transport buffers the whole stream; the lines arrive
        # together when the engine is done, which is fine for a harness.
        response = await http.post("/engine/invoke", json=body)
        lines = [json.loads(line) for line in response.text.splitlines() if line.strip()]
        for line in lines:
            if "thinking" in line:
                print("  event:", line["thinking"][:150])
        last = lines[-1] if lines else {}
        if "error" in last:
            print("terminal status: Error")
            print("category:", last["error"].get("error_category"))
            print("\nERROR MESSAGE:\n", last["error"].get("message", "")[:3000])
            return
        result = last.get("result", {})
        print("terminal status:", "Completed" if result.get("success") else "Error")
        print("category:", result.get("error_category"))
        artifacts = result.get("artifacts") or []
        print(f"engine artifacts: {len(artifacts)}")
        for a in artifacts:
            print("  ", a.get("type"), "|", (a.get("name") or "")[:70], "|", len(a.get("data") or ""), "bytes")
        out = pathlib.Path(os.environ.get("E2E_DUMP", "")) if os.environ.get("E2E_DUMP") else None
        if out:
            out.write_text(json.dumps(result, indent=1))
            print("dumped the engine result to", out)
        if not result.get("success"):
            print("\nERROR MESSAGE:\n", str(result.get("error", ""))[:3000])


asyncio.run(main())
