---
date: 2026-11-18
order: 1
---
## A nested agent in the desktop app follows its delegation grant

The desktop app runs a saved agent that the local agent calls as its own cloud turn. That turn did not read the delegation grant of the edge. With the delegation grant switch on (`ELITEA_DELEGATION_GRANTS_ENFORCED`, on by default since the delegation migration), the main service now applies the grant when the desktop starts the nested agent: the agent runs with what its caller and the approved grant cover, or is refused when the edge has no grant and it holds something its caller does not. The desktop transcript shows the same notice as the web app, with **Review delegation** for an editor. Client contract 1.61 (additive). Upgrade Main with the desktop app. See [Notices in the chat](/admin/enterprise-controls/least-privilege-and-trust#notices-in-the-chat).
