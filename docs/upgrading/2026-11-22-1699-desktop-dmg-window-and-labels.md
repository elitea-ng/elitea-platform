# The macOS disk image opens with no window bars and readable labels; a hand-run package needs a version (#1699)

- **What changed.**
  - **The disk image's window shows only the picture.** Finder saved the window with its tab bar on (macOS 26 and
    later), and its AppleScript cannot hide the tab bar, the path bar or the sidebar. `macos-dmg.sh` now rewrites the
    window settings in the image's `.DS_Store` (`apps/elitea-desktop/scripts/ds-store.mjs hide-chrome`) with the
    toolbar, status bar, tab bar, path bar and sidebar all off, and `dmg-verify.mjs` fails the build unless every one
    of them reads back off, in the writable image and again in the finished `.dmg`. A viewer who turned on Finder's
    **View › Show Tab Bar** still sees a tab bar: that setting is theirs and covers every Finder window.
  - **The backgrounds are light.** Finder draws the icon labels in dark text in light and dark mode, so the mid-tone
    purple of the first release hid them. `brand/elitea` and `brand/sample` are regenerated as soft tints (relative
    luminance 0.72 to 0.90) with a dark arrow; a test reads the committed PNGs and requires at least 7:1 for dark
    text on every pixel of both label rows (about 15:1 now). The drawn fallback for a brand without its own
    background is light too.
  - **`desktop-package.yml` by hand needs a `version`.** The `workflow_dispatch` gained a required `version` input,
    stamped into the app and the disk image's volume (`Elitea <version>`); a hand-run package can no longer carry the
    development `0.1.0`. Releases are unchanged: publish.yml passes the release's version.
- **Who is affected.**
  - People who open the macOS disk image: a plain window with the picture, the app and Applications, and labels that
    read.
  - Whoever runs **Desktop — package for device management** by hand: type the version in.
  - Brand repositories with their own `dmg-background*.png`: keep the picture light, or Finder's dark labels will not
    read on it (`scripts/brand/dmg.mjs --from --to` draws a light one).
- **What to do.**
  - Nothing for a release. When running the package workflow by hand, give the version (for example
    `gh workflow run desktop-package.yml -f version=7.9.0 -f keep_artifacts=true`).
  - A brand with a dark custom background: redraw it light, or delete both files to get the drawn one.
