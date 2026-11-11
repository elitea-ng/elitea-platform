# The desktop app checks a sub-agent's brief against what its agent saw (#1827, ADR-0050 decision 9d)

- **What changed.**
  - **Excerpts.** On a local turn, the desktop agent sees a `[message m-…]` line before each message it may quote:
    the current message (under the id of its `question_id`) and the person's and other participants' messages of a
    session seeded from the server (under the id of their message group). The id is the cloud's id for the same
    message. A `spawn_agent` excerpt is accepted when its quote is in the text after that line in the model's last
    request. The text of every message, steering messages included, is neutralized so that no message can show
    another message's id.
  - **Attachments.** Each image attached to the current message is shown to the agent as
    `[Attachment att-…: "name"]`. A brief may name it; the sub-agent gets the image's name only.
  - **Memory entries** are still refused with `BRIEF_MEMORY_NOT_RECALLED`: the desktop app has no `recall` tool.
  - **`subagent_started`** (desktop IPC) now carries `display_name`, `avatar`, `profile_digest`, `brief_digest` and
    `brief_refs`. It never carries the soul, a quote or a fact. The thread history keeps them.
  - No `ephemeral_agents` row is written for a local sub-agent.
- **Who is affected.**
  - People who use sub-agents on local turns in the desktop app.
  - Desktop sessions created before this release: their stored messages have no id line, so their older messages
    cannot be quoted until the session is seeded again.
- **What to do.**
  - Nothing. No server, migration or contract change: update the desktop app.
