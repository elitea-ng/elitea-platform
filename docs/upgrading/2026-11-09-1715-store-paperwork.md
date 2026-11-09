# Store paperwork per brand (#1715, ADR-0052 decision 10)

`apps/elitea-mobile/docs/store/` now holds ELITEA's store answers only, and
`docs/store/template/` is the set another brand starts from.

- **Who is affected:** brand owners preparing an App Store or Google Play
  submission.
- **What to do:** for ELITEA, nothing; the copyright line now names the
  manifest's publisher, still to be confirmed as the legal entity. Another brand
  copies the template, fills in its brand.json values and its own legal entity,
  and runs its own demo workspace for reviewers.
- **Behaviour:** none; documentation only. The review notes now say that
  managed configuration and Local Push are dormant without an MDM or a
  workspace policy. A test (`scripts/store-paperwork.test.mjs`) keeps ELITEA's
  pages to brand/elitea's identifiers and the template free of them.
