# Upgrade notes

One file per change that needs an operator to act on upgrade. A pull request
**adds** a file here and edits no shared list, so two open pull requests never
conflict over upgrade notes. (The old single file, `docs/UPGRADING.md`, holds the
history up to 2026-11-09 and takes no new entries.)

**Name:** `YYYY-MM-DD-<issue>-<slug>.md`. Use the date the pull request is opened
and the issue it closes, for example `2026-11-09-1697-desktop-mdm-packages.md`.
Sorting by name puts the newest file last. Read the files dated after the release
you are upgrading from.

**Content:** one `# ` title naming the area and the change, with the issue (and
the ADR, if there is one). Then say what changed, who is affected, and what to
do: the same three things a `docs/UPGRADING.md` entry said. If a chart guard or
a values comment needs to point at the note, name the file.

`scripts/ci/check-upgrading-notes.mjs` (the No Binaries workflow) enforces the
name and the title, and keeps `docs/UPGRADING.md` closed.
