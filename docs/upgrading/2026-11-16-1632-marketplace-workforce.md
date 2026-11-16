# Marketplace: workforce managers (ADR-0051 M6, issue 1632)

**What changed.** A new shared migration lets a workforce manager's Marketplace proposals live
beside the dismissal ladder's: `agent_workforce_proposals` gains the kinds `hire_listing` and
`feedback_report`, the columns `listing_id`, `listing_version_id` and `payload`, and nullable
`application_id` and `stage`. The one-open-proposal index now covers the ladder kinds only.
`agent_workforce_policies` gains `auto_feedback_enabled`, `auto_feedback_by`,
`listing_min_reviews`, `listing_min_rating` and `listing_max_dismissal_rate`. The number is
`0253` in the pull request and is renumbered above the head of `main` at merge. Agents with
`can_review_agents` get four tools (`marketplace_search`, `marketplace_listing`, `propose_hire`,
`suggest_feedback`) over one new claim-bound route of the runtime content service. Client
contract 1.56 adds two read operations to the client set (`listAgentWorkforceProposals`,
`getAgentWorkforce`) for the mobile app. No new permission: the proposals are decided with
`agents.workforce.manage`.

**Who is affected.** Every operator. Nothing changes for a project until a workforce manager
uses the new tools. Both automatic behaviours are off by default.

**What to do.** Run the migrations, then upgrade Main, the worker and the clients together:
**upgrade the worker with Main**, because the tools need the new route. No other action is
required, except for the two cases below.

**A caller of the report endpoint must change.** `POST
/elitea_core/marketplace_feedback_report/prompt_lib/{project}/{listing}` (introduced with
feedback in the previous release) now takes `{"proposal_id": "...", "text": "..."}` instead of
the agent, the hired agent, the outcome and the stage. A report publishes only from the
`feedback_report` proposal that the agent drafted with `suggest_feedback`, and the caller needs
`agents.workforce.manage`. A script that posted the old body gets 400. Nothing else calls it.

**The policy is replaced as a whole.** `PUT .../agent_workforce/prompt_lib/{project}/policy`
replaces the whole policy. A script that still sends only the six earlier fields resets
`auto_feedback_enabled` to off and the three listing numbers to their defaults (3 reviews, a
4.0 average rating, a dismissal rate under 0.2). Send the new fields to keep a changed value.
`auto_feedback_enabled` is off by default, and turning it on records the person who did so as
the approver of every automatic report (`policy:auto_feedback`).
