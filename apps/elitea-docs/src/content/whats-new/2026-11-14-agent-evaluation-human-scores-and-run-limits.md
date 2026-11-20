---
date: 2026-11-14
order: 2
---
## Agent Evaluation: human scores and run limits

A dimension that only a person can score can now be used in a run: the run answers each case and waits, the scorecard shows **Waiting for a human score**, and a reviewer scores each case with a note. Scores are kept as a history (a re-score adds one), the run's overall score is recalculated after each score, and a release gate with a human dimension stays pending until every case is scored. New permissions: evaluation human-score read (admins, editors, viewers) and create (admins, editors). A **Run limits** button on the Runs tab saves token and cost limits per case and per run, and whether a run **stops** or only **reports** when its run limit is reached; the run list shows which limit a run reached. **Run the migrations** (a tenant and a shared one), and upgrade Main and the web app together. See [Evaluation](/menus/agents#evaluation).
