---
date: 2026-11-20
---
## Python sandbox takes files, the internal tools are lighter, and tool schemas are fitted to the model's provider everywhere

The Python sandbox (`run_code`) now accepts a `files` argument when the agent also has Data analysis: up to 5 conversation attachments or artifact files (256 KiB each, 512 KiB together) arrive in the program's input as text or base64. The Data analysis and sub-agent tools use shorter descriptions, so an agent with every module on sends about 1,600 tokens less per turn, with the same tools and arguments; a test now holds each set to a stated budget. The tool-schema fitting that keeps a provider from refusing a tool (added for Gemini, llama.cpp, Anthropic, OpenAI and Bedrock) now also applies to the engines that call models directly (DeepWiki, Inventory) and is computed once per schema instead of on every model call. See [Python sandbox](/menus/internal-tools#python-sandbox-code-execution) and [What the modules cost the model](/menus/internal-tools#what-the-modules-cost-the-model).
