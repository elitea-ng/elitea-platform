# Desktop: trusted folders UI (#3062, ADR-0061)

**What changed.** The desktop app gains the person's side of trusted folders:
the **Trust this folder…** dialog (from a folder's menu, and from a Windows
`sandbox_unavailable` tool card), the **No sandbox** / **Trust paused** /
**Environment fallback** badge on the folder, the thread title and the
composer, Revoke from the badge or the menu, and **Settings › Trusted
folders**. **Settings › Approval rules** marks a rule remembered in a trusted
folder as **Needs trust** while the folder is not trusted, and a person's own
Allow rule can say **Without a sandbox, in a trusted folder** (`sandbox:
host`); a repository's rule never can.

**When it takes effect.** With the desktop release that carries it. The
dialog offers a confirmation only while the native client policy allows
trust (see the `local_work_folder_trust` note of #3059); otherwise it says
which policy key blocks it.

**What to do.** Nothing. Administrators who do not want trusted folders on a
deployment whose `local_work_max_sandbox_mode` is `full-access` turn
`local_work_folder_trust` off (**Admin > Features and configuration**).
