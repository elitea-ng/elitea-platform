# Web brand: colour roles and hand-tuned legacy ids (#3091, ADR-0062 decision 1.4)

**What changed.** The default brand pack gains the 27 brand-guide colour roles
as ids `role.<name>` in both schemes (`role.text`, `role.textMuted`, …). The
408 legacy ids stay. No component reads a role yet, so nothing on screen
changes in this release.

A role takes the colour of one generic legacy id that a deployment can tune
in `scheme_tokens`. The map is
`apps/elitea-web/src/shared/brand/tokens/legacy-roles.json`:

| Role                  | Legacy id            |
| --------------------- | -------------------- |
| `role.bg`             | `background.default` |
| `role.border`         | `border.lines`       |
| `role.text`           | `text.secondary`     |
| `role.textMuted`      | `text.primary`       |
| `role.primary`        | `primary.main`       |
| `role.primaryHover`   | `primary.hover`      |
| `role.primaryPressed` | `primary.pressed`    |
| `role.signal`         | `text.link`          |
| `role.success`        | `success.main`       |
| `role.warning`        | `warning.main`       |
| `role.danger`         | `text.error`         |
| `role.dangerFill`     | `error.main`         |
| `role.onDangerFill`   | `error.contrastText` |

The rule: a stated `role.*` id wins when it is tuned. Else the legacy id of
its row wins when it is tuned. A value is tuned when it differs from the
value the id gets when the pack does not state it. That value is the
default, re-hued from `brand.hue`. So a legacy id pinned at its default
under another hue keeps its colour in the role. A role with no tuned id is
derived from `brand.hue`, like every other id. `brand.onBrand` gives its
colour to `role.onPrimary`, as it does to `primary.contrastText`.

**Who is affected.** No deployment loses a colour in this release. The other
14 roles have no legacy id. The ids that are not on the map (for example
`text.default`, `focus.ring`, `text.button.primary` and component ids such as
`background.tab.hover`) do not reach a role. A tuned one keeps its colour on
each screen that reads it. A screen that a later release moves onto a shared
component reads roles, and the tuned id then stops reaching that screen.

**What to do.** Nothing.

**Branding packages.** A package downloaded before this release states the
408 earlier ids. If every id has the product default's value, import and
restore pin no scheme tokens, as before. A package that states fewer ids,
or a tuned id, is carried into `scheme_tokens`, as before.

**Downgrading elitea-main.** The earlier release has no `role.*` ids and
ignores a stated one. Its import compares the number of ids. So it refuses a
package with the default colours downloaded from this release: the 435 ids
then reach `scheme_tokens`, which takes six-digit hex only. A tuned package
that states only the tuned ids still imports. After a downgrade, import
packages downloaded from the earlier release.
