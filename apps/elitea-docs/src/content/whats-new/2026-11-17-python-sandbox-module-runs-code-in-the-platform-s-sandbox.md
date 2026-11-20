---
date: 2026-11-17
order: 4
---
## Python sandbox module runs code in the platform's sandbox

The **Python sandbox** module of an agent now works. It gives the agent a `run_code` tool that runs a short program in the same isolated sandbox as pipeline Code nodes, in the languages the deployment's sandbox serves (Python, JavaScript, TypeScript and Rust). Python returns what the program printed, its last expression and any traceback. The switch is enabled when the deployment has a code sandbox, and shows "No code sandbox is configured for this deployment" when it has none; the agent still runs either way. `GET /elitea_core/runtime_capabilities` now lists `code_sandbox_languages`. `run_code` is a `code_execution` tool for approval rules. Upgrade Main, the worker and the web app together. See [Internal tools](/menus/internal-tools#python-sandbox-code-execution).
