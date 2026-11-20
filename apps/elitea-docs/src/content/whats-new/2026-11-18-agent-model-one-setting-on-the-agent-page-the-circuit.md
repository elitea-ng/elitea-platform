---
date: 2026-11-18
order: 10
---
## Agent model: one setting on the agent page; the circuit breaker no longer blames agents for refused runs

On the agent page, the model picker in the chat now shows the model chosen in the agent's settings, and a model picked in the chat changes the setting (and the unsaved-changes state), instead of being a second, unconnected picker. The circuit breaker no longer counts a run that the platform refused (for example "The execution input is invalid.") as a failure of the agent, and it resumes by itself an agent that it stopped only because of such runs. The placeholder text of the **Generate with AI** boxes (skill, agent, project context) is pinned to the theme's placeholder colour in both themes.
