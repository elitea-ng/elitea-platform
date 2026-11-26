# Brand tokens: `motion.avatar` and `motion.handoff` (#3066, ADR-0061)

**What changed.** The design tokens (`libs/ts/client-core/src/brand/tokens.json`)
gain `motion.avatar` (one duration and loop flag per agent avatar state:
`idle`, `thinking`, `speaking`, `working`, `waiting`, `waking`, and `oneShot`
for success and error) and `motion.handoff` (the hand-off baton, 400 ms on
`motion.easing`). The avatar motion of the web, desktop and mobile clients now
reads its timings from these tokens instead of literals. The default values are
the timings the avatars always had, so nothing looks different.

**Who is affected.** Brands built from their own copy of `tokens.json`
(ADR-0052 brand template). Workspace brand packs (ADR-0024) carry no motion and
are unchanged.

**What to do.** Nothing. A brand that wants other avatar timings sets the new
keys in its `tokens.json` and runs `node libs/ts/client-core/scripts/gen-brand.mjs`.
