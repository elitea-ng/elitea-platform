# Agents: file format version key and stricter import validation (#3137)

**What changed.** Agent export writes `schema: elitea.agent/v1` as the first key of an `.agent.md` file, and a
team export writes `schema: elitea.team/v1` in `team.yaml`. The files now carry the reasoning effort, tags, memory
settings, heartbeat schedule, tool envelope, workforce capabilities, run caps, output contract, citations rule, the
avatar, and the agent's evaluation datasets. An agent with uploaded avatar files exports as `<name>.agent.zip`
instead of `.agent.md`. Files without the key still import as before. A file with another format version is refused.

Import (JSON, Markdown and team zip) now checks every agent before it writes anything, with the rules of a save in
the agent editor: `memory`, `context`, `proactivity`, `tool_envelope`, `capabilities`, `run_caps` (never above the
platform's cap for a new agent's risk tier), `output_contract`, `citations`, the persona (16 KiB), the avatar and its
files (2 MiB, 512 px, 4 s, type read from the bytes), variables, tags, evaluation datasets and memory seeds. The first
problem answers **422** with `code: import_invalid` and the `field` it found, for example
`applications[0].versions[0].meta.run_caps.max_tool_calls`. Before, some of these values were stored as they were,
and an agent with such a value failed at its first chat turn; others refused only that agent and answered 207.
Import still never takes credentials: toolkits arrive without them, to be connected after the import.

A new read-only route, `GET /elitea_core/run_caps/prompt_lib/{project_id}/{application_id}`, gives the agent editor
the platform's run caps for the agent's risk tier. It needs the agent read permission.

**Who is affected.** Anyone who imports agent files made by hand or by an older export, and scripts that call the
import route and read its 201/207/400 answer.

**What to do.** Nothing for files that this platform exported. A file that now answers 422 holds a value the agent
could not have run with: fix the named field, or remove it, and import again. A script that calls the import route
must treat 422 as a refusal of the whole file; nothing was written. No migration.
