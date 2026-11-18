# Delegation: a refused or narrowed delegation shows in the chat (issue 2161)

**What changed.** The worker sends two new run events, `delegation_refused` and `delegation_narrowed`,
next to the result the model reads when a saved agent or pipeline called as a tool is refused (no
approved grant, outside the run principal's bound, past the depth limit) or runs without what its
grant no longer covers. The main service keeps them on the answer (`metadata.delegation_notices_v1`,
at most 16, no migration) and serves them with `can_review` and the people to ask for the person who
reads. The web, desktop and mobile apps show the notice at the called agent. Client contract 1.60 adds
the two frames and `delegation_notices_v1`; both are additive.

**Who is affected.** Projects whose agents call saved agents or pipelines. Nothing is refused that was
not refused before, and no setting is added: the notice only shows what the run already did.

**What to do.** Upgrade the main service and the workers (the chart's default order). Nothing else.
A worker from an earlier release sends no notice, so nothing shows. A main service from an earlier
release does not keep or stamp the notice: it shows while the answer streams and is gone after a
reload, and **Review delegation** is not offered until the main service is upgraded. Older mobile and
desktop builds ignore the new frames. Answers written before the upgrade keep no notice.
