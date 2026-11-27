---
date: 2026-11-27
---
## Imported agents keep their model, and clearer team import and edit dialogs

An imported agent now runs on the model its file names, so it keeps that model's context window. Before, an agent from a blueprint or from another installation ran on the project's default model, with that model's smaller context window, and nothing said so. Import now gives each agent a model of this installation: the model in its file when this installation has it, else this project's default model. After the import, the dialog says which model each agent runs on and warns about every agent that got the default. Agents imported before this release keep running on the project's default model: import them again to give them their file's model. See [Import and export entities](/how-tos/agents-pipelines/import-export).

**Import team** now shows the team's name and description, its agents with their avatars (including the zip's own avatar files), their models and the **Lead** badge, a summary of what the file carries, and one **Before you import** callout. A single agent's import shows its avatar, description and model. While the import runs, the dialog says what it is doing and stays open until it ends. **Edit team** lists each agent with its avatar and the **Lead** badge, and every button there has a tooltip and is named for screen readers. **Add an agent** shows the agents' avatars. See [Teams](/how-tos/agents-pipelines/agent-teams).
