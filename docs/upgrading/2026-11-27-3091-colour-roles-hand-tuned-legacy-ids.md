# Web brand: colour roles and hand-tuned legacy ids (#3091, ADR-0062 decisions 1.4 and 1.6)

**What changed.** The default brand pack gains the 27 brand-guide colour roles
as ids `role.<name>` in both schemes (`role.text`, `role.textMuted`, …). The
408 legacy ids stay. No component reads a role yet, so nothing on screen
changes in this release.

13 roles take the colour of one generic legacy id each, which a deployment
can tune in `scheme_tokens`. The map is
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

`brand.onBrand` gives its colour to a label role when its fill resolves to the
colour of `primary.main`: with the default colours, `role.onPrimary`, and in
the light scheme `role.onSignal`. A tuned label keeps its own colour. It always
gives its colour to `primary.contrastText`.

**Who is affected.** No deployment loses a colour in this release. The other
14 roles have no generic legacy id. Every id that is not on the map does not
reach a role: for example `text.default`, `focus.ring`, `text.button.primary`,
`secondary.main` and component ids such as `background.tab.hover`. A tuned
one keeps its colour on each screen that reads it. A later release can move
a screen's reads off such an id, onto a role or onto a shared component's
own id. The tuned id then stops reaching that screen, and that release says
so in its own upgrade note.

`focus.ring` loses nothing: code reads it for focus rings (ADR-0062
R-T15), so a tuned ring reaches moved screens too.

A file layer (`BRAND_PACK_PATH`) can state every legacy id with another
`brand.hue`. It then keeps the default look only if it also states the 27
`role.*` ids of `default.pack.json`. Then change a role and the legacy id
of its row together: under another hue, a stated role wins over its legacy id.

**Tuning a role.** A package can state a `role.*` id in `scheme_tokens`. Give
the legacy id of its row the same colour. A later release moves the reads of
that legacy id to the role, and those screens then take the role's colour. In
`scheme_tokens`, `role.hover`, `role.selected` and `role.scrim` take a
six-digit hex colour only, so a tuned one is opaque.

**What to do.** Nothing on upgrade. After a downgrade of elitea-main, download
a new package from the Branding page, or restore a version imported before the
upgrade (see below).

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
only. A package that states only its changed ids still imports.
