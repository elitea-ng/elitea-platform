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
value that the id gets when the pack does not state it: its default, re-hued
from `brand.hue`. So a legacy id pinned at its default under another hue
keeps its colour in the role. A role with no tuned id is derived from
`brand.hue`, like every other id.

`brand.onBrand` gives its colour to `role.onPrimary`, and in the light scheme
to `role.onSignal`, when the pack does not tune that id. It always gives its
colour to `primary.contrastText`.

**Who is affected.** No deployment loses a colour in this release. The other
14 roles have no generic legacy id. Every id that is not on the map does not
reach a role: for example `text.default`, `focus.ring`, `text.button.primary`,
`secondary.main` and component ids such as `background.tab.hover`. A tuned
one keeps its colour on each screen that reads it. A screen that a later
release moves onto a shared component reads roles, and the tuned id then
stops reaching that screen.

**Tuning a role.** A package can state a `role.*` id in `scheme_tokens`. Give
the legacy id of its row the same colour: a later release moves the reads of
that legacy id to the role, and those screens then take the role's colour.
`role.hover`, `role.selected` and `role.scrim` take a six-digit hex colour
only, so a tuned one is opaque.

**What to do.** Nothing on upgrade. Before you downgrade elitea-main, keep a
branding package downloaded from the earlier release (see below).

**Branding packages.** A package downloaded before this release states the
408 earlier ids. If every id has the product default's value, import and
restore pin no scheme tokens, as before. A record that states every id at the
product default is untouched also under another hue, as before: its colours
then re-hue. A record with an id changed from the product default goes into
`scheme_tokens` whole, for both schemes, as before. Every value there must be
six-digit hex, so a full package with one colour changed is refused: state
only the ids that you change.

**Downgrading elitea-main.** The earlier release has no `role.*` ids and
ignores a stated one. Its import compares the number of ids. So it refuses
import and restore of a package with the default colours downloaded from this
release: the 435 ids then reach `scheme_tokens`, which takes six-digit hex
only. A package that states only its changed ids still imports. After a
downgrade, import or restore packages downloaded from the earlier release.
