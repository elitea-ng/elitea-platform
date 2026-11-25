# Web brand: colour roles and hand-tuned legacy ids (ADR-0062 decision 1.4)

**What changed.** The default brand pack gains the 27 brand-guide colour roles
as ids `role.<name>` in both schemes (`role.text`, `role.textMuted`, …), beside
the 408 legacy ids, which stay. Nothing on screen changes in this release: no
component reads a role yet. Later releases (ADR-0062 stage 4) move the web
app's reads of the generic legacy ids below onto their roles, one folder per
release.

A pack that hand-tunes a generic legacy id in `scheme_tokens` keeps that colour:
the role takes it. The map is
`apps/elitea-web/src/shared/brand/tokens/legacy-roles.json`:

| Role                  | Legacy ids, in precedence order |
| --------------------- | ------------------------------- |
| `role.bg`             | `background.default`            |
| `role.border`         | `border.lines`                  |
| `role.text`           | `text.secondary`                |
| `role.textMuted`      | `text.primary`, `text.default`  |
| `role.primary`        | `primary.main`                  |
| `role.primaryHover`   | `primary.hover`                 |
| `role.primaryPressed` | `primary.pressed`               |
| `role.signal`         | `text.link`, `focus.ring`       |
| `role.success`        | `success.main`                  |
| `role.warning`        | `warning.main`                  |
| `role.danger`         | `text.error`                    |
| `role.dangerFill`     | `error.main`                    |
| `role.onDangerFill`   | `error.contrastText`            |

A role takes the first value the pack states differently from the product
default: the `role.*` id itself, then each legacy id of its row in order. If
none differs, the role is derived from `brand.hue` like every other id. The
other 14 roles have no generic legacy id; component ids (for example
`background.tab.hover`) are not on the map and keep tuning their own
component. Stage 4 rewrites only reads of the ids on the map.

**Who is affected.** Only a deployment that tuned **two ids of one row to two
different colours**: `text.primary` and `text.default`, or `text.link` and
`focus.ring`. The role takes the first id of the row, so after the stage 4
release of a folder, the places that read the second id show the first id's
colour. Every other deployment, including one that states only `brand.hue`
or tunes one id per row, keeps its colours.

**What to do.** Before you take the stage 4 releases, download the branding
package (Admin › Branding › Download branding package) and check the
`scheme_tokens` of both schemes in it for those two pairs.
If the two ids of a pair differ, choose the colour for the role and state it
as the `role.*` id (`role.textMuted` or `role.signal`): a stated role wins over
every legacy id. No migration and nothing to do now otherwise.

**Branding packages.** A package downloaded before this release states the
408 earlier ids. Import and restore treat it as before: a record whose ids
all have the product default's values pins no scheme tokens, so the 27 new
ids it lacks do not make it count as hand-tuned.

**Rollback.** The previous release has no `role.*` ids and ignores a stated
one. Its import still compares the number of ids, so it refuses a package
downloaded from this release with the default colours (the 435 ids then
reach `scheme_tokens`, which takes six-digit hex only). After a rollback,
import packages downloaded from the earlier release.
