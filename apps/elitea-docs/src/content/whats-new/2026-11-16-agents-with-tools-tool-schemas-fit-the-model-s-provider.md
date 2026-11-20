---
date: 2026-11-16
order: 12
---
## Agents with tools: tool schemas fit the model's provider

An agent with toolkit tools, a sub-agent, an MCP server or the internal tools selected could fail with **The model service rejected this request**, while the same agent with no tools answered. The cause was a tool's argument schema that the model's provider refuses: `additionalProperties` and `default` on Gemini, a `$ref`, `const` or a root-level `anyOf` that Anthropic or OpenAI-compatible servers refuse, and patterns and size bounds that llama.cpp cannot turn into a grammar. The worker now rewrites each tool's schema for the provider family of the selected model before it sends the request: references are inlined, `const` and `oneOf` are rewritten, and a constraint the provider cannot carry (a pattern, a length bound) moves into the argument's description so the model still reads it. The tool still checks every call against its full schema, and no required argument is dropped. A model whose name is not recognised gets the strictest portable form. See [Troubleshooting](/support/troubleshooting/).
