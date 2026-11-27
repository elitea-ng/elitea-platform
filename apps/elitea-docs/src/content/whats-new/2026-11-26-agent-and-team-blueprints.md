---
date: 2026-11-26
---
## Start from a blueprint, and hire teams in the Marketplace

**Start from a blueprint** on the Agents page opens a gallery of ready-made agents and teams: Research Scout, Writing Editor, Data Analyst, Daily Ops Digest, Code Reviewer and Agent Smith, and the teams Content Studio, Incident Desk and the Elitea dev team. **Import** creates one as it is. **Craft from it** opens Build with AI with the blueprint's brief, so you can make your own version. Every release attaches each blueprint as one file that the **Import** button reads, and the docs site has the same files. On the mobile app, **Create an agent** has the same gallery with **Craft from it**. See [Start from a blueprint](/how-tos/agents-pipelines/blueprints).

Agent files now carry everything the agent editor sets: the memory settings, heartbeat schedule, tool envelope, capabilities, run caps, output contract, citations, persona, avatar and evaluation datasets. The file names its format version (`elitea.agent/v1`). An agent with avatar files exports as an `.agent.zip`. Import checks every value before it creates anything and names the field that it refuses. Import never carries credentials and never turns a heartbeat on. See [Agent and team file format](/reference/agent-file-format).

The agent editor now sets these values without JSON: **Memory settings** on the **Memory** tab, **Heartbeat schedule** on the **Heartbeat** tab, **Capabilities and tool envelope** on the **Workforce** tab, and **Run caps** on the **Configuration** tab.

A saved team can now be published to the Marketplace and hired like an agent. **Publish to Marketplace** in the team card's **⋯** menu lists it when every member agent is published. **Hire** on a team listing installs or forks every member and saves the team in your project. See [Teams](/menus/marketplace#teams).
