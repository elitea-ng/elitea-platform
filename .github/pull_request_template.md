## Summary

<!-- What changes and why. Write in ASD-STE100 (AGENTS.md). -->

## Checks

<!-- The commands you ran and their results. -->

## Checklist

- [ ] A user-facing client feature changed: I updated `docs/client-feature-parity.md` for web, desktop and mobile (`shipped`, `planned (#issue)` or `n-a: reason`).
- [ ] An operator must act on upgrade: I added a new file `docs/upgrading/<issue>-<slug>.md` (`docs/upgrading/README.md`), and did not edit `docs/UPGRADING.md` or another pull request's note.
- [ ] Logic that two clients need is in `libs/ts/client-core` when it is pure and the same on both, not copied into each app.
