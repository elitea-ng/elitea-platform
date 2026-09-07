#!/usr/bin/env python3
# coding=utf-8

"""
Subprocess worker for Ask tool.

This module is executed in a separate process to isolate the Ask operation
from the main web worker process, following the same pattern as wiki generation.

Unlike wiki generation, this does NOT use worker slot limits since Ask is fast.

Contract:
- Reads JSON from --input
- Writes JSON to --output
- Emits progress logs to stdout (parsed by parent for thinking steps)

Input JSON schema:
{
    "base_path": "/path/to/cache",
    "question": "User's question",
    "llm_settings": {...},
    "embedding_model": {...},
    "github_repository": "owner/repo",
    "k": 15,
    "chat_history": []  // optional
}

Output JSON schema:
{
    "success": bool,
    "answer": str,
    "sources": [...],
    "thinking_steps": [...],
    "error": str (when success=false)
}
"""

import argparse
import asyncio
import json
import logging
import os
import sys
import traceback
from typing import Any, Dict, List, Optional


def _configure_logging() -> None:
    """Configure logging for the subprocess."""
    level_name = os.getenv("DEEPWIKI_WORKER_LOG_LEVEL", "INFO").upper()
    level = getattr(logging, level_name, logging.INFO)

    handler = logging.StreamHandler(sys.stdout)
    handler.setFormatter(
        logging.Formatter("%(asctime)s | %(levelname)s | %(name)s -- %(message)s")
    )

    root = logging.getLogger()
    root.handlers = [handler]
    root.setLevel(level)

    # Keep noisy libraries at WARNING
    if level > logging.DEBUG:
        for noisy in ["httpx", "urllib3", "openai", "langchain", "langgraph", "sentence_transformers"]:
            logging.getLogger(noisy).setLevel(logging.WARNING)


def _print(msg: str) -> None:
    """Print with flush for real-time streaming."""
    sys.stdout.write(msg + "\n")
    sys.stdout.flush()


def _emit_event(event_type: str, data: dict) -> None:
    """Emit a structured event for the UI."""
    import json
    from datetime import datetime
    event = {
        "event": event_type,
        "data": data,
        "timestamp": datetime.now().isoformat()
    }
    _print(f"[ASK_EVENT] {json.dumps(event)}")


def _emit_thinking_step(step_type: str, title: str, content: str, metadata: Optional[Dict] = None) -> None:
    """Emit a structured thinking step for the UI (agentic mode)."""
    step = {
        "type": step_type,
        "title": title,
        "content": content[:500] if content else "",
        "metadata": metadata or {}
    }
    _print(f"[THINKING_STEP] {json.dumps(step)}")


def _answer_streaming(llm_settings: Dict[str, Any]) -> bool:
    """Whether the model streams its answer (issue #701).

    ON by default. The agentic Ask loop already reads the model through
    LangGraph's `messages` stream, so this turns one whole message into a
    run of fragments the engine publishes as `llm_chunk` events, and the
    wiki chat shows the answer as it is written. A deployment whose model
    endpoint cannot stream turns it off with `llm_settings.streaming:
    false` rather than with a code change.
    """
    return bool(llm_settings.get("streaming", True))


def _build_llm_and_embeddings(llm_settings: Dict[str, Any], embedding_model: Any):
    """Build LLM and embeddings from settings."""
    from langchain_openai import ChatOpenAI
    from langchain_openai.embeddings import OpenAIEmbeddings

    # Detect provider
    provider = llm_settings.get("provider", "openai")
    
    # Normalize field names
    api_base = llm_settings.get("api_base") or llm_settings.get("openai_api_base")
    api_key = llm_settings.get("api_key") or llm_settings.get("openai_api_key")
    model_name = llm_settings.get("model_name", "gpt-4o-mini")
    organization = llm_settings.get("organization")
    max_retries = llm_settings.get("max_retries", 2)
    max_tokens = llm_settings.get("max_tokens", 4096)  # Lower for Ask responses
    default_headers = llm_settings.get("default_headers", {})

    if not api_base:
        raise ValueError("llm_settings.api_base is required")
    if not api_key:
        raise ValueError("llm_settings.api_key is required")

    if provider == "anthropic":
        from langchain_anthropic import ChatAnthropic

        anthropic_base_url = api_base.rstrip("/")
        if anthropic_base_url.endswith("/v1"):
            anthropic_base_url = anthropic_base_url[:-3]

        if not default_headers:
            default_headers = {
                "openai-organization": str(organization) if organization else "",
                "Authorization": f"Bearer {api_key}"
            }

        llm = ChatAnthropic(
            model=model_name,
            api_key=api_key,
            base_url=anthropic_base_url,
            max_tokens=max_tokens,
            temperature=0.1,
            max_retries=max_retries,
            streaming=_answer_streaming(llm_settings),
            default_headers=default_headers,
        )
        embeddings_base_url = api_base if api_base.endswith("/v1") else api_base.rstrip("/") + "/v1"
    else:
        # OpenAI
        temperature = 1.0 if str(model_name).startswith("o") else 0.1
        
        llm = ChatOpenAI(
            model=model_name,
            temperature=temperature,
            api_key=api_key,
            base_url=api_base,
            organization=organization,
            max_retries=max_retries,
            streaming=_answer_streaming(llm_settings),
            max_tokens=max_tokens,
        )
        embeddings_base_url = api_base

    # Build embeddings
    embedding_model_name = embedding_model if isinstance(embedding_model, str) else "text-embedding-3-large"
    if isinstance(embedding_model, dict):
        embedding_model_name = embedding_model.get("model_name", embedding_model_name)

    embeddings = OpenAIEmbeddings(
        model=embedding_model_name,
        openai_api_key=api_key,
        openai_api_base=embeddings_base_url,
        openai_organization=organization,
    )

    return llm, embeddings


async def run_ask_agentic_async(payload: Dict[str, Any]) -> Dict[str, Any]:
    """
    Execute the Agentic Ask engine with the given payload.

    This is the new agentic workflow (DEEPWIKI_ASK_AGENTIC=1):
    - Uses progressive disclosure tools (search_symbols → get_relationships → get_code)
    - Multi-step agent with tool calling (LangGraph/DeepAgents)
    - Streams thinking steps via [THINKING_STEP] markers

    Args:
        payload: Input configuration (same schema as run_ask)

    Returns:
        Result dictionary with answer, thinking_steps, etc.
    """
    from .vectorstore import VectorStoreManager
    from .retrievers import WikiRetrieverStack
    from .repository_analysis_store import RepositoryAnalysisStore
    from .ask_engine import create_ask_engine

    logger = logging.getLogger(__name__)

    base_path = payload.get("base_path", "")
    question = payload.get("question", "")
    llm_settings = payload.get("llm_settings", {})
    embedding_model = payload.get("embedding_model")

    # Support new multi-provider repo_config with legacy fallback
    repo_config = payload.get("repo_config", {})
    if repo_config:
        provider_type = repo_config.get("provider_type", "github")
        repository = repo_config.get("repository", "")
        branch = repo_config.get("branch", "main")
    else:
        provider_type = "github"
        repository = payload.get("github_repository", "")
        branch = payload.get("github_branch", "main")

    chat_history = payload.get("chat_history", [])
    repo_identifier_override = payload.get("repo_identifier_override")
    analysis_key_override = payload.get("analysis_key_override")

    if not question:
        return {"success": False, "error": "No question provided"}
    if not repository:
        return {"success": False, "error": "No repository specified"}

    from .repository_identity import build_query_repo_identifier
    repo_identifier = build_query_repo_identifier(
        repository=repository,
        branch=branch,
        repo_config=repo_config,
    )
    _print(f"[AGENTIC ASK] Processing question for: {repo_identifier} (provider: {provider_type})")
    _emit_thinking_step("start", "Agentic Ask Started", f"Question: {question[:200]}...")

    # Set up cache directories
    cache_dir = os.path.join(base_path, "cache") if base_path else os.path.expanduser("~/.elitea/wiki_indexes")
    model_cache_dir = os.path.join(base_path, "huggingface_cache") if base_path else None

    if model_cache_dir:
        os.environ['TRANSFORMERS_CACHE'] = model_cache_dir
        os.environ['HF_HOME'] = model_cache_dir
        os.environ['SENTENCE_TRANSFORMERS_HOME'] = model_cache_dir

    collected_events = []
    final_answer = ""

    try:
        # In K8s Jobs mode, ensure indexes are available locally.
        # Jobs+API: downloads from platform bucket using llm_settings creds.
        # In Docker mode: ArtifactManager returns True immediately.
        try:
            from .artifact_manager import ArtifactManager, is_jobs_mode
            if is_jobs_mode():
                _print("[ask] K8s Jobs mode — verifying indexes are available locally...")
                from .artifacts_platform_client import (
                    create_platform_client_from_llm_settings, get_artifact_bucket,
                )
                from .registry_manager import normalize_wiki_id
                _art_client = create_platform_client_from_llm_settings(llm_settings) if llm_settings else None
                art_mgr = ArtifactManager(
                    cache_dir=cache_dir,
                    artifacts_client=_art_client,
                    bucket=get_artifact_bucket() if _art_client else "",
                )
                # Prefer the manifest-selected canonical id. ADO raw repo fields can be
                # only the repository name, while generated artifacts are keyed by
                # provider-normalized org/project/repo.
                _index_repo_id = (
                    repo_identifier_override.strip()
                    if isinstance(repo_identifier_override, str) and repo_identifier_override.strip()
                    else repo_identifier
                )
                _wiki_id = normalize_wiki_id(_index_repo_id)
                ok = art_mgr.ensure_indexes_for_wiki(wiki_id=_wiki_id)
                if ok:
                    _print("[ask] Index verification: OK")
                else:
                    _print("[ask] Warning: index verification returned False — will try loading anyway")
        except Exception as _art_err:
            _print(f"[ask] Warning: ArtifactManager check failed: {_art_err}")

        # Build LLM and embeddings
        _print("Initializing LLM and embeddings...")
        llm, embeddings = _build_llm_and_embeddings(llm_settings, embedding_model)

        # Resolve canonical repo identifier
        canonical_repo_identifier = repo_identifier
        try:
            from .repo_resolution import (
                cache_index_has_repo,
                load_cache_index,
                resolve_canonical_repo_identifier,
                resolve_unified_db_path,
            )

            if isinstance(repo_identifier_override, str) and repo_identifier_override.strip():
                override_candidate = repo_identifier_override.strip()
                idx = load_cache_index(cache_dir)
                if cache_index_has_repo(idx, override_candidate):
                    canonical_repo_identifier = override_candidate
                    _print(f"Using repo_identifier_override: {canonical_repo_identifier}")
                else:
                    canonical_repo_identifier = override_candidate
                    _print(f"Using repo_identifier_override not yet registered in cache index: {canonical_repo_identifier}")
            else:
                canonical_repo_identifier = resolve_canonical_repo_identifier(
                    repo_identifier=repo_identifier,
                    cache_dir=cache_dir,
                    repositories_dir=os.path.join(cache_dir, 'repositories'),
                )
        except ValueError as e:
            return {
                "success": False,
                "error": str(e),
                "error_type": "ValueError",
                "error_category": "invalid_input",
            }
        except Exception:
            canonical_repo_identifier = repo_identifier

        # Phase 6: Always prefer UnifiedRetriever (UnifiedWikiDB is the
        # single retrieval store).
        _unified_retriever_enabled = True
        _unified_retriever_active = False
        retriever_stack = None
        query_service = None

        if _unified_retriever_enabled:
            try:
                from .unified_retriever import UnifiedRetriever
                from .unified_db import UnifiedWikiDB
                from .code_graph.storage_query_service import StorageQueryService

                _udb_path = resolve_unified_db_path(
                    canonical_repo_id=canonical_repo_identifier,
                    cache_dir=cache_dir,
                )

                if _udb_path and os.path.isfile(_udb_path):
                    _udb = UnifiedWikiDB(_udb_path, readonly=True)
                    _embed_fn = None
                    if embeddings and hasattr(embeddings, 'embed_query'):
                        _embed_fn = embeddings.embed_query
                    retriever_stack = UnifiedRetriever(
                        db=_udb,
                        embedding_fn=_embed_fn,
                        embeddings=embeddings,
                    )
                    query_service = StorageQueryService(_udb)
                    _unified_retriever_active = True
                    _print(
                        "[UNIFIED_RETRIEVER] Agentic Ask using UnifiedRetriever "
                        f"for {canonical_repo_identifier} from {_udb_path}"
                    )
                else:
                    _print(
                        "[UNIFIED_RETRIEVER] No .wiki.db found for "
                        f"{canonical_repo_identifier} — falling back to legacy retriever"
                    )
            except Exception as _ur_exc:
                _print(f"[UNIFIED_RETRIEVER] Upgrade failed, using legacy: {_ur_exc}")

        # Load vector store (skip hard failure when UnifiedRetriever is active)
        vectorstore_manager = None
        vectorstore = None
        if not _unified_retriever_active:
            _print("Loading vector store...")
            vectorstore_manager = VectorStoreManager(
                cache_dir=cache_dir,
                model_cache_dir=model_cache_dir,
                embeddings=embeddings
            )
            vectorstore = vectorstore_manager.load_by_repo_name(canonical_repo_identifier)
            if vectorstore is None:
                return {
                    "success": False,
                    "error": f"No wiki index found for {repo_identifier}. Please generate a wiki first."
                }
            _print(f"Vector store loaded with {vectorstore.index.ntotal} vectors")
        else:
            _print("[UNIFIED_RETRIEVER] Skipping legacy FAISS vectorstore loading")

        graph_manager = None
        relationship_graph = None
        if _unified_retriever_active:
            _print("[UNIFIED_RETRIEVER] Using UnifiedWikiDB query service; skipping legacy relationship graph loading")
        else:
            _print("Unified DB not active — proceeding without graph analysis")

        # Unified DB has its own FTS5 table; no legacy graph text index is loaded.

        # Load repository analysis
        _print("Loading repository analysis...")
        analysis_store = RepositoryAnalysisStore(cache_dir=cache_dir)
        repository_analysis = ""
        if isinstance(analysis_key_override, str) and analysis_key_override.strip():
            repository_analysis = analysis_store.get_analysis_for_prompt(
                canonical_repo_identifier,
                analysis_key_override=analysis_key_override,
            )
        if not repository_analysis:
            repository_analysis = analysis_store.get_analysis_for_prompt(canonical_repo_identifier)
        if not repository_analysis:
            repository_analysis = analysis_store.get_analysis_for_prompt(repository)

        if repository_analysis:
            _print(f"Repository analysis loaded ({len(repository_analysis):,} chars)")
        else:
            _print("No repository analysis found")

        # Initialize retriever stack (legacy fallback if UnifiedRetriever not active)
        if retriever_stack is None:
            _print("Initializing legacy retriever stack...")
            retriever_stack = WikiRetrieverStack(
                vectorstore_manager=vectorstore_manager,
                relationship_graph=relationship_graph,
                use_enhanced_graph=True
            )

        # Build repo analysis dict
        repo_analysis_dict = {"summary": repository_analysis} if repository_analysis else None

        # Create agentic Ask engine
        _print("Creating agentic Ask engine (progressive disclosure tools)...")
        _emit_thinking_step("engine", "Ask Engine", "Initializing agent with progressive disclosure tools...")

        engine = create_ask_engine(
            retriever_stack=retriever_stack,
            graph_manager=graph_manager,
            code_graph=relationship_graph,
            repo_analysis=repo_analysis_dict,
            llm_client=llm,
            llm_settings=llm_settings,
            query_service=query_service,
            enable_graph_analysis=query_service is not None or relationship_graph is not None,
        )

        # Run agentic Ask and collect events
        _print("Starting agentic Ask loop...")
        async for event in engine.ask(question, chat_history=chat_history or None):
            event_type = event.get('event_type', '')
            data = event.get('data', {})

            collected_events.append(event)

            if event_type == 'thinking_step':
                step = data
                step_type = step.get('type', 'step')

                if step_type == 'tool_call':
                    tool_name = step.get('tool', 'unknown')
                    tool_input = step.get('input', '')
                    call_id = step.get('tool_call_id') or step.get('call_id') or ''
                    _emit_thinking_step(
                        'tool_call',
                        f"Calling: {tool_name}",
                        tool_input[:500],
                        {"tool": tool_name, "step": step.get('step', 0), "call_id": call_id}
                    )
                    # Also emit as ASK_EVENT for backward compatibility
                    _emit_event("tool_start", {
                        "tool": tool_name,
                        "input": tool_input[:200],
                        "status": "in_progress",
                    })

                elif step_type == 'tool_result':
                    tool_name = step.get('tool', 'unknown')
                    preview = step.get('output_preview', step.get('output', '')[:500])
                    output_length = step.get('output_length', len(str(preview)))
                    call_id = step.get('tool_call_id') or step.get('call_id') or ''
                    _emit_thinking_step(
                        'tool_result',
                        f"Result ({output_length} chars)",
                        preview[:500] if preview else '',
                        {"tool": tool_name, "step": step.get('step', 0), "call_id": call_id}
                    )
                    _emit_event("tool_end", {
                        "tool": tool_name,
                        "status": "completed",
                        "output": preview[:200] if preview else '',
                    })

            elif event_type == 'llm_chunk':
                fragment = data.get('text') or ''
                if fragment:
                    _print(f"[LLM_CHUNK] {json.dumps({'text': fragment})}")

            elif event_type == 'ask_complete':
                final_answer = data.get('answer', '')
                _print(f"Agentic Ask complete: {len(final_answer)} chars, {data.get('steps', 0)} steps")

            elif event_type == 'ask_error':
                error = data.get('error', 'Unknown error')
                _emit_thinking_step('error', 'Error', error)
                return {
                    "success": False,
                    "error": error,
                    "thinking_steps": [e for e in collected_events if e.get('event_type') == 'thinking_step']
                }

        _emit_thinking_step("complete", "Ask Complete", f"Generated {len(final_answer)} character answer")

        return {
            "success": True,
            "answer": final_answer,
            "sources": [],  # Agentic mode embeds citations in the answer text
            "thinking_steps": [
                e.get('data', {}) for e in collected_events
                if e.get('event_type') == 'thinking_step'
            ],
            "query_used": f"[agentic] {question[:200]}",
            "documents_retrieved": 0,  # Not applicable for agentic mode
            "agentic": True,
        }

    except Exception as e:
        logger.exception("Agentic Ask failed")
        _emit_thinking_step('error', 'Ask Failed', str(e))
        return {
            "success": False,
            "error": f"Agentic Ask error: {str(e)}\n{traceback.format_exc()}"
        }


def run_ask(payload: Dict[str, Any]) -> Dict[str, Any]:
    """
    Execute the Ask tool with the given payload.

    When DEEPWIKI_ASK_AGENTIC=1, uses the new agentic engine with
    progressive disclosure tools (search_symbols → get_relationships → get_code).
    Otherwise, falls back to the original 2-phase non-agentic workflow.
    
    Args:
        payload: Input configuration
        
    Returns:
        Result dictionary
    """
    # Feature flag: agentic Ask mode
    _agentic = os.environ.get("DEEPWIKI_ASK_AGENTIC", "").strip()
    if _agentic in ("1", "true", "yes"):
        _print("[ASK] Using agentic mode (DEEPWIKI_ASK_AGENTIC=1)")
        return asyncio.run(run_ask_agentic_async(payload))

    from .vectorstore import VectorStoreManager
    from .retrievers import WikiRetrieverStack
    from .repository_analysis_store import RepositoryAnalysisStore
    from .ask_tool import (
        AskTool, 
        ASK_CONTEXT_TOKEN_BUDGET,
        ASK_QUERY_OPTIMIZATION_OUTPUT_TOKENS,
        ASK_ANSWER_OUTPUT_TOKENS
    )
    
    logger = logging.getLogger(__name__)
    
    base_path = payload.get("base_path", "")
    question = payload.get("question", "")
    llm_settings = payload.get("llm_settings", {})
    embedding_model = payload.get("embedding_model")
    
    # Support new multi-provider repo_config with legacy fallback
    repo_config = payload.get("repo_config", {})
    if repo_config:
        provider_type = repo_config.get("provider_type", "github")
        repository = repo_config.get("repository", "")
        branch = repo_config.get("branch", "main")
    else:
        # Legacy GitHub-only mode
        provider_type = "github"
        repository = payload.get("github_repository", "")
        branch = payload.get("github_branch", "main")
    
    k = payload.get("k", 15)
    chat_history = payload.get("chat_history", [])
    repo_identifier_override = payload.get("repo_identifier_override")
    analysis_key_override = payload.get("analysis_key_override")
    
    if not question:
        return {"success": False, "error": "No question provided"}
    
    if not repository:
        return {"success": False, "error": "No repository specified"}
    
    from .repository_identity import build_query_repo_identifier
    repo_identifier = build_query_repo_identifier(
        repository=repository,
        branch=branch,
        repo_config=repo_config,
    )
    
    _print(f"Processing question for repository: {repo_identifier} (provider: {provider_type})")
    
    # Set up cache directories
    cache_dir = os.path.join(base_path, "cache") if base_path else os.path.expanduser("~/.elitea/wiki_indexes")
    model_cache_dir = os.path.join(base_path, "huggingface_cache") if base_path else None

    # Resolve canonical commit-scoped identifier where possible (or use override).
    canonical_repo_identifier = repo_identifier
    try:
        from .repo_resolution import (
            cache_index_has_repo,
            load_cache_index,
            resolve_canonical_repo_identifier,
            resolve_unified_db_path,
        )

        if isinstance(repo_identifier_override, str) and repo_identifier_override.strip():
            # Validate override exists in cache before using it
            override_candidate = repo_identifier_override.strip()
            idx = load_cache_index(cache_dir)
            if cache_index_has_repo(idx, override_candidate):
                canonical_repo_identifier = override_candidate
                _print(f"Using repo_identifier_override: {canonical_repo_identifier}")
            else:
                canonical_repo_identifier = override_candidate
                _print(f"Using repo_identifier_override not yet registered in cache index: {canonical_repo_identifier}")
        else:
            canonical_repo_identifier = resolve_canonical_repo_identifier(
                repo_identifier=repo_identifier,
                cache_dir=cache_dir,
                repositories_dir=os.path.join(cache_dir, 'repositories'),
            )

        # Log exact cache keys used (FAISS/docs + graph) for deterministic testing.
        try:
            idx = load_cache_index(cache_dir)
            faiss_key = idx.get(canonical_repo_identifier)
            unified_idx = idx.get('unified_db', {}) if isinstance(idx.get('unified_db', {}), dict) else {}
            unified_key = unified_idx.get(canonical_repo_identifier)

            if isinstance(faiss_key, str):
                _print(
                    f"[cache] faiss_key={faiss_key} files={faiss_key}.faiss,{faiss_key}.docs.pkl "
                    f"repo_id={canonical_repo_identifier}"
                )
            else:
                _print(f"[cache] faiss_key not found for repo_id={canonical_repo_identifier}")

            if isinstance(unified_key, str):
                _print(
                    f"[cache] unified_db_key={unified_key} file={unified_key}.wiki.db "
                    f"repo_id={canonical_repo_identifier}"
                )
            else:
                _print(f"[cache] unified_db_key not found for repo_id={canonical_repo_identifier}")
        except Exception:
            pass
    except ValueError as e:
        return {
            "success": False,
            "error": str(e),
            "error_type": "ValueError",
            "error_category": "invalid_input",
        }
    except Exception:
        canonical_repo_identifier = repo_identifier
    
    # Set HuggingFace cache env vars
    if model_cache_dir:
        os.environ['TRANSFORMERS_CACHE'] = model_cache_dir
        os.environ['HF_HOME'] = model_cache_dir
        os.environ['SENTENCE_TRANSFORMERS_HOME'] = model_cache_dir
    
    try:
        # Build LLM and embeddings
        _print("Initializing LLM and embeddings...")
        llm, embeddings = _build_llm_and_embeddings(llm_settings, embedding_model)
        
        # Initialize vector store manager
        _print("Loading vector store...")
        vectorstore_manager = VectorStoreManager(
            cache_dir=cache_dir,
            model_cache_dir=model_cache_dir,
            embeddings=embeddings
        )
        
        # Try to load existing vector store using repo_identifier (matches wiki generation cache key)
        vectorstore = vectorstore_manager.load_by_repo_name(canonical_repo_identifier)
        if vectorstore is None:
            return {
                "success": False,
                "error": f"No wiki index found for {repo_identifier}. Please generate a wiki first using the 'Generate Wiki' button."
            }
        
        _print(f"Vector store loaded with {vectorstore.index.ntotal} vectors")
        
        relationship_graph = None
        _print("Proceeding without legacy relationship graph loading")
        
        # Load repository analysis (FULL, not truncated) for query optimization
        _print("Loading repository analysis...")
        analysis_store = RepositoryAnalysisStore(cache_dir=cache_dir)
        
        # Prefer version-pinned analysis when requested.
        repository_analysis = ""
        if isinstance(analysis_key_override, str) and analysis_key_override.strip():
            repository_analysis = analysis_store.get_analysis_for_prompt(
                canonical_repo_identifier,
                analysis_key_override=analysis_key_override,
            )
            if not repository_analysis:
                _print(
                    f"Pinned analysis not found for key '{analysis_key_override}'. "
                    "Falling back to latest analysis for canonical repo id."
                )

        if not repository_analysis:
            # Try with canonical identifier first (repo:branch:commit8), then fallback to repo-only.
            repository_analysis = analysis_store.get_analysis_for_prompt(canonical_repo_identifier)
        if not repository_analysis:
            # Fallback: try without branch suffix (legacy cache files)
            _print(f"Analysis not found for '{repo_identifier}', trying fallback to '{repository}'...")
            repository_analysis = analysis_store.get_analysis_for_prompt(repository)
        
        if repository_analysis:
            _print(f"Repository analysis loaded ({len(repository_analysis):,} chars)")
        else:
            _print("No repository analysis found - query optimization will be skipped")
        
        # Initialize retriever stack with graph
        _print("Initializing retriever stack...")

        # Phase 6: Always prefer UnifiedRetriever (UnifiedWikiDB is the
        # single retrieval store).
        _unified_retriever_enabled = True
        retriever_stack = None
        if _unified_retriever_enabled:
            try:
                from .unified_retriever import UnifiedRetriever
                from .unified_db import UnifiedWikiDB

                _udb_path = resolve_unified_db_path(
                    canonical_repo_id=canonical_repo_identifier,
                    cache_dir=cache_dir,
                )

                if _udb_path and os.path.isfile(_udb_path):
                    _udb = UnifiedWikiDB(_udb_path, readonly=True)
                    _embed_fn = None
                    if embeddings and hasattr(embeddings, 'embed_query'):
                        _embed_fn = embeddings.embed_query
                    retriever_stack = UnifiedRetriever(
                        db=_udb,
                        embedding_fn=_embed_fn,
                        embeddings=embeddings,
                    )
                    _print(
                        "[UNIFIED_RETRIEVER] Ask using UnifiedRetriever "
                        f"for {canonical_repo_identifier} from {_udb_path}"
                    )
                else:
                    _print(
                        "[UNIFIED_RETRIEVER] No .wiki.db found for "
                        f"{canonical_repo_identifier} — falling back to legacy retriever"
                    )
            except Exception as _ur_exc:
                _print(f"[UNIFIED_RETRIEVER] Upgrade failed, using legacy: {_ur_exc}")

        if retriever_stack is None:
            retriever_stack = WikiRetrieverStack(
                vectorstore_manager=vectorstore_manager,
                relationship_graph=relationship_graph,
                use_enhanced_graph=True
            )
        
        # Thinking callback to emit progress
        def thinking_callback(step: Dict[str, Any]):
            msg = step.get("message", step.get("type", ""))
            _print(f"[THINKING] {msg}")
        
        # Initialize Ask tool with token budgets
        _print(f"Initializing Ask tool (context: {ASK_CONTEXT_TOKEN_BUDGET:,} tokens, query_opt: {ASK_QUERY_OPTIMIZATION_OUTPUT_TOKENS:,} tokens, answer: {ASK_ANSWER_OUTPUT_TOKENS:,} tokens)...")
        ask_tool = AskTool(
            retriever_stack=retriever_stack,
            llm_client=llm,
            repository_analysis=repository_analysis,
            thinking_callback=thinking_callback,
            max_context_tokens=ASK_CONTEXT_TOKEN_BUDGET,
            optimize_query=bool(repository_analysis),
            query_output_tokens=ASK_QUERY_OPTIMIZATION_OUTPUT_TOKENS,
            answer_output_tokens=ASK_ANSWER_OUTPUT_TOKENS
        )
        
        # Emit query optimization event
        _emit_event("tool_start", {
            "tool": "query_optimization",
            "input": question,
            "status": "in_progress",
            "description": "Optimizing search query based on repository context..."
        })
        
        # Execute query
        _print("Searching and generating answer...")
        if chat_history:
            response = ask_tool.ask_with_history(question, chat_history, k=k)
        else:
            response = ask_tool.ask(question, k=k)
        
        # Emit query optimization complete
        _emit_event("tool_end", {
            "tool": "query_optimization",
            "status": "completed",
            "output": f"Optimized query: {response.query_used[:200]}..." if len(response.query_used) > 200 else f"Optimized query: {response.query_used}"
        })
        
        # Emit answer generation event
        _emit_event("tool_start", {
            "tool": "answer_generation",
            "input": f"Retrieved {response.documents_retrieved} documents",
            "status": "in_progress",
            "description": "Generating comprehensive answer from retrieved context..."
        })
        
        _emit_event("tool_end", {
            "tool": "answer_generation",
            "status": "completed",
            "output": f"Generated answer ({len(response.answer)} chars, {len(response.sources)} sources)"
        })
        
        _print(f"Answer generated ({len(response.answer)} chars, {len(response.sources)} sources)")
        
        return {
            "success": True,
            "answer": response.answer,
            "sources": [
                {
                    "index": src.index,
                    "source": src.source,
                    "symbol": src.symbol,
                    "type": src.chunk_type
                }
                for src in response.sources
            ],
            "thinking_steps": response.thinking_steps,
            "query_used": response.query_used,
            "documents_retrieved": response.documents_retrieved
        }
        
    except Exception as e:
        logger.exception("Ask tool failed")
        return {
            "success": False,
            "error": f"Ask tool error: {str(e)}\n{traceback.format_exc()}"
        }


def main(argv: Optional[List[str]] = None) -> int:
    """Main entry point for subprocess."""
    _configure_logging()
    logger = logging.getLogger(__name__)
    
    parser = argparse.ArgumentParser(description="Ask subprocess worker")
    parser.add_argument("--input", required=True, help="Path to input JSON")
    parser.add_argument("--output", required=True, help="Path to output JSON")
    args = parser.parse_args(argv)
    
    # Read input
    try:
        with open(args.input, "r", encoding="utf-8") as f:
            payload = json.load(f)
    except Exception as e:
        logger.error(f"Failed to read input: {e}")
        result = {"success": False, "error": f"Failed to read input: {e}"}
        with open(args.output, "w", encoding="utf-8") as f:
            json.dump(result, f)
        return 1
    
    # Run ask
    try:
        result = run_ask(payload)
    except Exception as e:
        logger.exception("Unhandled exception in ask worker")
        result = {"success": False, "error": str(e)}
    
    # Write output
    try:
        with open(args.output, "w", encoding="utf-8") as f:
            json.dump(result, f, indent=2, ensure_ascii=False)
    except Exception as e:
        logger.error(f"Failed to write output: {e}")
        return 1
    
    return 0 if result.get("success") else 1


if __name__ == "__main__":
    sys.exit(main())
