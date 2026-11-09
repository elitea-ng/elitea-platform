---
title: "elitea-main: applied migrations were reworded in their comments"
issue: 1405
date: 2026-10-23
order: 28
---

**Affects:** nobody has to act. This entry records why a ledger can now hold
a checksum, or a name, that no file in the release has.

**What changed.** 66 applied migrations (52 shared, 0049 to 0159, and 14
tenant, 0122 to 0151) had the legacy runtime's name removed from their
comments. Two were renamed as well: shared `0091` is now
`0091_viewer_secret_list_parity` and tenant `0124` is now
`0124_elitea_tools_legacy_shape`. The executable SQL of every one is
unchanged, and a unit test (`TestReleasedTextsDifferOnlyInComments`) proves
it.

**What the runner does.** `internal/infra/db/migrate/released.go` lists the
released checksum of each reworded version. A ledger row holding that
checksum counts as applied, under its released name or the new one, and does
not run again. A fresh install records the new text. Any other checksum is
still refused with `checksum mismatch`, as for every other migration. This
mirrors what `libs/rust/pg-migrate` (`apply_with_rewrites`) already does for
the engines' rewritten migrations.
