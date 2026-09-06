"""
Agentic Ask Engine — LangGraph-based Q&A with progressive disclosure tools.

Optimised for Q&A (not long-form research):
- Fewer iterations (max 8 vs 15 for deep research)
- No TodoListMiddleware, no FilesystemMiddleware, no SubAgentMiddleware
  → only graph/search tools (search_symbols, get_relationships, get_code,
    search_docs, query_graph, think)
- Uses create_agent (not create_deep_agent) with minimal middleware:
  SummarizationMiddleware + PatchToolCallsMiddleware + AnthropicPromptCachingMiddleware
- Streams events compatible with the deep-research UI

Gated behind DEEPWIKI_ASK_AGENTIC=1 feature flag.
"""

import logging
import os
from typing import Any, Optional, Dict, AsyncGenerator, List
from dataclasses import dataclass
from datetime import datetime

from langchain_core.language_models import BaseChatModel
from langchain_core.messages import AIMessage, ToolMessage

logger = logging.getLogger(__name__)

# Safety limits — Ask is lighter than deep research
ASK_MAX_ITERATIONS = int(os.environ.get("DEEPWIKI_ASK_MAX_ITERATIONS", "8"))

# Feature flag
ASK_AGENTIC_ENABLED = os.environ.get("DEEPWIKI_ASK_AGENTIC", "").strip() in ("1", "true", "yes")


@dataclass
class AskConfig:
    """Configuration for an agentic Ask session."""
    max_iterations: int = ASK_MAX_ITERATIONS
    max_search_results: int = 20
    enable_graph_analysis: bool = True


class AskEngine:
    """
    Agentic Q&A engine using DeepAgents (LangGraph-based).

    Creates an agent with progressive disclosure tools that follows
    the workflow: search_symbols → get_relationships → get_code → answer.

    Events are streamed via LangGraph's astream with dual mode
    (messages + updates), matching the deep research pattern for
    UI compatibility.

    Usage:
        engine = AskEngine(
            retriever_stack=retriever_stack,
            graph_manager=graph_manager,
            code_graph=code_graph,
            llm_client=llm,
        )

        # Async streaming
        async for event in engine.ask("How does auth work?"):
            handle_event(event)

        # Synchronous convenience
        answer = engine.ask_sync("How does auth work?")
    """

    def __init__(
        self,
        retriever_stack: Any,   # WikiRetrieverStack
        graph_manager: Any,     # GraphManager
        code_graph: Any,        # NetworkX graph
        repo_analysis: Optional[Dict] = None,
        llm_client: Optional[BaseChatModel] = None,
        backend: Optional[object] = None,
        llm_settings: Optional[Dict] = None,
        query_service: Any = None,
        config: Optional[AskConfig] = None,
    ):
        """
        Initialize the Ask engine.

        Args:
            retriever_stack: WikiRetrieverStack for vector search
            graph_manager: GraphManager for relationship graph + FTS5
            code_graph: Loaded NetworkX code graph
            repo_analysis: Pre-loaded repository analysis dict
            llm_client: Pre-built LangChain chat model (preferred)
            backend: DeepAgents backend factory or instance
            llm_settings: LLM config (used only if llm_client is None)
            config: Ask configuration
        """
        self.retriever_stack = retriever_stack
        self.graph_manager = graph_manager
        self.code_graph = code_graph
        self.repo_analysis = repo_analysis or {}
        self.llm_client = llm_client
        self.backend = backend
        self.llm_settings = llm_settings or {}
        self.query_service = query_service
        self.config = config or AskConfig()

        # Session state
        self.final_answer: str = ""
        self.status: str = "idle"
        self.error: Optional[str] = None

    def _build_model(self) -> BaseChatModel:
        """Build the LLM from settings.

        Always uses ChatOpenAI because we go through a LiteLLM proxy
        that handles auth with JWT tokens and routes to the backend.
        """
        api_base = self.llm_settings.get("api_base") or self.llm_settings.get("openai_api_base")
        api_key = self.llm_settings.get("api_key") or self.llm_settings.get("openai_api_key")
        model_name = self.llm_settings.get("model_name", "gpt-4o-mini")
        max_tokens = self.llm_settings.get("max_tokens", 8192)

        from langchain_openai import ChatOpenAI
        return ChatOpenAI(
            model=model_name,
            api_key=api_key,
            base_url=api_base,
            temperature=0.0,
            max_tokens=max_tokens,
        )

    def _create_agent(self, system_prompt: str):
        """
        Create a LangGraph agent with **only** graph/search tools.

        Uses ``create_agent`` (not ``create_deep_agent``) so that
        TodoListMiddleware, FilesystemMiddleware, and SubAgentMiddleware
        are NOT included — Ask only needs codebase search tools.

        Middleware kept:
        - SummarizationMiddleware — auto-summarise when context exceeds limit
        - AnthropicPromptCachingMiddleware — save tokens on Anthropic models
        - PatchToolCallsMiddleware — fix malformed tool calls
        """
        from langchain.agents import create_agent
        from langchain.agents.middleware.summarization import SummarizationMiddleware
        from langchain_anthropic.middleware import AnthropicPromptCachingMiddleware
        from deepagents.middleware.patch_tool_calls import PatchToolCallsMiddleware
        from .deep_research.research_tools import create_codebase_tools

        model = self.llm_client or self._build_model()

        # Get FTS5 index from graph_manager if available
        fts_index = getattr(self.graph_manager, "fts_index", None) if self.graph_manager else None

        # Force progressive tools for the agentic Ask
        _orig = os.environ.get("DEEPWIKI_PROGRESSIVE_TOOLS", "")
        os.environ["DEEPWIKI_PROGRESSIVE_TOOLS"] = "1"
        try:
            custom_tools = create_codebase_tools(
                retriever_stack=self.retriever_stack,
                graph_manager=self.graph_manager,
                code_graph=self.code_graph,
                repo_analysis=self.repo_analysis,
                event_callback=None,  # Events come from LangGraph stream
                graph_text_index=fts_index,
                query_service=self.query_service,
            )
        finally:
            # Restore original env
            if _orig:
                os.environ["DEEPWIKI_PROGRESSIVE_TOOLS"] = _orig
            else:
                os.environ.pop("DEEPWIKI_PROGRESSIVE_TOOLS", None)

        # Compute summarization defaults matching deepagents/graph.py logic:
        # If the model exposes a profile with max_input_tokens use fraction-based
        # triggers, otherwise fall back to fixed token counts.
        _profile = getattr(model, "profile", None)
        if isinstance(_profile, dict) and isinstance(_profile.get("max_input_tokens"), int):
            trigger = ("fraction", 0.85)
            keep = ("fraction", 0.10)
        else:
            trigger = ("tokens", 170_000)
            keep = ("messages", 6)

        # Minimal middleware — no filesystem, no todos, no subagents
        agent_middleware = [
            SummarizationMiddleware(
                model=model,
                trigger=trigger,
                keep=keep,
                trim_tokens_to_summarize=None,
            ),
            AnthropicPromptCachingMiddleware(unsupported_model_behavior="ignore"),
            PatchToolCallsMiddleware(),
        ]

        agent = create_agent(
            model=model,
            tools=custom_tools,
            system_prompt=system_prompt,
            middleware=agent_middleware,
        )

        return agent

    async def ask(
        self,
        question: str,
        chat_history: Optional[List[Dict[str, str]]] = None,
        session_id: Optional[str] = None,
    ) -> AsyncGenerator[Dict, None]:
        """
        Answer a question about the repository using agentic tool calling.

        Streams events for UI display, matching the deep research event format.

        Args:
            question: The user's question
            chat_history: Optional list of {"role": "user"|"assistant", "content": "..."}
            session_id: Optional session identifier

        Yields:
            Event dictionaries:
            - ask_start: session begins
            - thinking_step: tool_call / tool_result events
            - ask_complete: final answer
            - ask_error: on failure
        """
        from .ask_prompts import get_ask_instructions, get_ask_prompt

        session_id = session_id or datetime.now().strftime("%Y%m%d_%H%M%S")
        self.status = "running"
        self.final_answer = ""
        self.error = None
        step_count = 0
        # The fragments of the final answer, in arrival order, and the
        # tool calls already announced. See the two streamed branches
        # below (issue #701).
        answer_fragments = []
        announced_tool_calls = set()

        yield {
            "event_type": "ask_start",
            "data": {
                "session_id": session_id,
                "question": question,
                "timestamp": datetime.now().isoformat(),
            },
        }

        try:
            # Build system prompt
            system_prompt = get_ask_instructions(max_iterations=self.config.max_iterations)

            # Build user prompt with repo context + history
            repo_context = self._get_repo_context()
            history_str = ""
            if chat_history:
                history_str = "\n".join(
                    f"{'User' if m['role'] == 'user' else 'Assistant'}: {m['content'][:300]}"
                    for m in chat_history[-4:]
                )

            user_prompt = get_ask_prompt(
                question=question,
                repo_context=repo_context,
                chat_history=history_str,
            )

            # Create the agent
            agent = self._create_agent(system_prompt)

            # Stream with dual mode
            stream_input = {"messages": [{"role": "user", "content": user_prompt}]}

            async for chunk in agent.astream(
                stream_input,
                stream_mode=["messages", "updates"],
            ):
                if not isinstance(chunk, tuple) or len(chunk) != 2:
                    continue

                stream_mode, data = chunk

                # UPDATES stream — we mostly ignore todos for Ask,
                # but still forward if present for UI compatibility
                if stream_mode == "updates":
                    if isinstance(data, dict):
                        for _node_name, node_data in data.items():
                            if isinstance(node_data, dict) and "todos" in node_data:
                                yield {
                                    "event_type": "todo_update",
                                    "data": {
                                        "todos": node_data["todos"],
                                        "timestamp": datetime.now().isoformat(),
                                    },
                                }

                # MESSAGES stream — tool calls and final answer
                elif stream_mode == "messages":
                    if not isinstance(data, tuple) or len(data) != 2:
                        continue

                    message, metadata = data

                    if isinstance(message, AIMessage):
                        if hasattr(message, "tool_calls") and message.tool_calls:
                            for tool_call in message.tool_calls:
                                # STREAMED (issue #701): with `streaming` on a
                                # tool call can reach this loop more than once,
                                # as the chunks of its arguments arrive, and one
                                # call would become a run of identical cards in
                                # the thinking log. A repeat of an id already
                                # announced is dropped. A call with NO id is
                                # never suppressed, because there is nothing to
                                # tell two of them apart by, and with streaming
                                # off every id is announced once — so this is
                                # inert on the path it replaces.
                                announced = tool_call.get("id")
                                if announced:
                                    if announced in announced_tool_calls:
                                        continue
                                    announced_tool_calls.add(announced)
                                step_count += 1
                                tool_call_id = tool_call.get("id", f"call_{step_count}")
                                tool_name = tool_call.get("name", "unknown")
                                yield {
                                    "event_type": "thinking_step",
                                    "data": {
                                        "step": step_count,
                                        "type": "tool_call",
                                        "tool": tool_name,
                                        "tool_call_id": tool_call_id,
                                        "call_id": tool_call_id,
                                        "input": str(tool_call.get("args", {}))[:500],
                                        "timestamp": datetime.now().isoformat(),
                                    },
                                }

                        # Final content (no tool calls = final answer)
                        if message.content and not message.tool_calls:
                            # STREAMED (issue #701). With `streaming` on, the
                            # answer reaches this branch one fragment at a
                            # time, so the assignment this replaces would keep
                            # the LAST fragment and throw the answer away. The
                            # fragments are joined instead, and each is
                            # published as its own event so that a reader can
                            # watch the answer being written. With streaming
                            # off there is exactly one fragment and the join
                            # is the assignment it replaces.
                            if isinstance(message.content, str):
                                answer_fragments.append(message.content)
                                self.final_answer = "".join(answer_fragments)
                                yield {
                                    "event_type": "llm_chunk",
                                    "data": {
                                        "text": message.content,
                                        "timestamp": datetime.now().isoformat(),
                                    },
                                }
                            else:
                                # NOT TEXT — an Anthropic content-block list,
                                # say. Assigned whole, exactly as before: a
                                # stringified block list is not an answer, and
                                # joining the fragments of one would compound
                                # that rather than fix it.
                                self.final_answer = message.content

                    elif isinstance(message, ToolMessage):
                        step_count += 1
                        content_str = str(message.content) if message.content else ""
                        tool_name = getattr(message, "name", None) or metadata.get("tool", "tool")
                        yield {
                            "event_type": "thinking_step",
                            "data": {
                                "step": step_count,
                                "type": "tool_result",
                                "tool": tool_name,
                                "tool_call_id": message.tool_call_id,
                                "call_id": message.tool_call_id,
                                "output_length": len(content_str),
                                "output_preview": content_str[:300] + "..." if len(content_str) > 300 else content_str,
                                "output": content_str[:500],
                                "timestamp": datetime.now().isoformat(),
                            },
                        }

            self.status = "completed"

            yield {
                "event_type": "ask_complete",
                "data": {
                    "session_id": session_id,
                    "answer": self.final_answer,
                    "steps": step_count,
                    "timestamp": datetime.now().isoformat(),
                },
            }

        except Exception as e:
            logger.error(f"Agentic Ask failed: {e}", exc_info=True)
            self.status = "failed"
            self.error = str(e)

            yield {
                "event_type": "ask_error",
                "data": {
                    "session_id": session_id,
                    "error": str(e),
                    "timestamp": datetime.now().isoformat(),
                },
            }

    def _get_repo_context(self) -> str:
        """Build repository context string from analysis."""
        parts = []

        if self.repo_analysis.get("summary"):
            parts.append(f"Repository Summary: {self.repo_analysis['summary']}")

        if self.repo_analysis.get("key_components"):
            parts.append(f"Key Components: {self.repo_analysis['key_components']}")

        if self.repo_analysis.get("languages"):
            parts.append(f"Languages: {self.repo_analysis['languages']}")

        return "\n".join(parts) if parts else "No repository overview available."

    def ask_sync(
        self,
        question: str,
        chat_history: Optional[List[Dict[str, str]]] = None,
        on_event: Optional[Any] = None,
    ) -> str:
        """
        Synchronous wrapper for the agentic Ask.

        Args:
            question: User's question
            chat_history: Optional conversation history
            on_event: Optional callback for events

        Returns:
            Final answer string
        """
        import asyncio

        async def _run():
            answer = ""
            async for event in self.ask(question, chat_history=chat_history):
                if on_event:
                    on_event(event)
                if event.get("event_type") == "ask_complete":
                    answer = event["data"].get("answer", "")
            return answer

        return asyncio.run(_run())


def create_ask_engine(
    retriever_stack: Any,
    graph_manager: Any,
    code_graph: Any,
    repo_analysis: Optional[Dict] = None,
    llm_client: Optional[BaseChatModel] = None,
    backend: Optional[object] = None,
    llm_settings: Optional[Dict] = None,
    query_service: Any = None,
    **kwargs,
) -> AskEngine:
    """
    Factory function to create an AskEngine.

    Args:
        retriever_stack: WikiRetrieverStack for codebase search
        graph_manager: GraphManager for relationship analysis
        code_graph: Loaded NetworkX code graph
        repo_analysis: Pre-loaded repository analysis
        llm_client: Pre-built LangChain chat model
        backend: DeepAgents backend factory or instance
        llm_settings: LLM configuration
        **kwargs: Additional config options

    Returns:
        Configured AskEngine instance
    """
    config = AskConfig(**{k: v for k, v in kwargs.items() if hasattr(AskConfig, k)})

    return AskEngine(
        retriever_stack=retriever_stack,
        graph_manager=graph_manager,
        code_graph=code_graph,
        repo_analysis=repo_analysis,
        llm_client=llm_client,
        backend=backend,
        llm_settings=llm_settings,
        query_service=query_service,
        config=config,
    )
