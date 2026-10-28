# Elitea AI assets

The app's fixed identity is the Elitea AI logo mark (LE-03, brand guide v3), the same artwork
as the web app and the desktop app (`apps/elitea-desktop/src-tauri/icons/`); its canonical
source is `libs/ts/client-core/src/brand/logo-mark.svg`. Rules for using it are in `docs/design-system.md` §5.

A workspace's own branding (its brand pack: accent, logo, fonts) is applied at runtime by
`src/theme/` and never changes these files: icons are fixed binaries (an OS rule), and the mark
is never recoloured with the workspace brand.

## In the app

The mark is drawn as vector art by `<EliteaMark variant size />` (`src/components/EliteaMark.tsx`)
from `ELITEA_MARK_SVG` in `@elitea/client-core/brand`. Its one canonical source is
`libs/ts/client-core/src/brand/logo-mark.svg`; `libs/ts/client-core/scripts/gen-brand.mjs`
derives the string the app imports and the web app's copies from it, and
`node libs/ts/client-core/scripts/gen-brand.mjs --check` fails in CI when any copy drifts.
Variants: `mark` (the mark alone) and `avatar` (the mark in a circle on the theme surface, the
primary agent's fallback avatar).

## App icon (wired in `app.config.ts`)

The artwork is the LE-03 mark of brand guide v3 (four flat rounded layers: cyan, violet + an
orange dot, indigo; 1:1) in its **colored-dark** treatment: the mark on Ink `#0B0B18`. The
approved LE-03 icon pack (variants colored-light, colored-dark, bw, inverted; per platform
macos/, windows/, android/, ios/, web/, github/) is the source of the opaque rasters; the
transparent layers are rendered from `libs/ts/client-core/src/brand/logo-mark.svg` so they are
sharp at 1024 and sized for the platform safe zones.

| File | Pixels / alpha | Key | Source |
|---|---|---|---|
| `assets/elitea.icon/` (`icon.json` + `Assets/elitea.png`) | Icon Composer document | `ios.icon`. The mark layer (744 px of 1024, the pack's iOS proportion) on a solid Ink fill (`"fill": {"solid": …}`, no gradient, no dark specialisation: the icon is dark in every appearance); the system derives Dark, Tinted and Clear. Needs Xcode 26+. | layer rendered from `logo-mark.svg` |
| `assets/images/icon.png` | 1024, opaque (no alpha channel) | `icon` (Android legacy, Expo fallback, store listing source) | pack `colored-dark/ios/…-ios-1024.png`, flattened to RGB |
| `assets/images/android-icon-foreground.png` | 1024 (= 108 dp), alpha | `android.adaptiveIcon.foregroundImage`. The mark, 436 px square, every pixel within 0.29 of the canvas from the centre (the 66 dp safe circle = 0.306), so a circular launcher mask never clips its corners. | rendered from `logo-mark.svg` |
| `assets/images/android-icon-background.png` | 1024, opaque | `android.adaptiveIcon.backgroundImage`: solid Ink; `backgroundColor: '#0B0B18'`. | solid fill (= pack `android-background-432`) |
| `assets/images/android-icon-monochrome.png` | 1024, alpha | `android.adaptiveIcon.monochromeImage` (Android 13+ themed icons): the mark in white on transparent, the foreground's size and position. | rendered from `logo-mark.svg` |
| `assets/images/favicon.png` | 48, opaque | `web.favicon` | pack `colored-dark/web/…-web-192.png`, downscaled |

There is one icon. The app had an alternate "workspace" icon under its former mascot branding;
it was removed together with that branding.

The pack's own `android/…-foreground-432.png` sizes the mark's square to the 66 dp *square*
(0.618 of the canvas); its corners then reach 0.42 of the canvas and a circular launcher mask
(radius 0.33) cuts them off, so this app renders the foreground at the safe-circle size instead.

## Splash

| File | Pixels | Key |
|---|---|---|
| `assets/images/splash-icon.png` | 1024, alpha | `expo-splash-screen` `image` and `dark.image`. The colour mark, every pixel within 0.30 of the canvas (Android 12+ masks to a circle of radius 1/3). |

Backgrounds: light Paper `#FFFFFF`, dark Ink `#0B0B18` (the `background` tokens), so the splash
hands over to the first screen without a flash. The colour mark reads on both.

## Regenerate

1. Render the mark with transparency at 2048 px, in colour and recoloured (every `fill="#…"`
   replaced by `#FFFFFF` for the monochrome layer), e.g.
   `inkscape logo-mark.svg --export-type=png --export-width=2048 --export-background-opacity=0 --export-filename=mark.png`.
2. Crop to the visible pixels and scale so the farthest visible pixel (alpha > 16) from the
   centre is the stated reach of the canvas (0.29 Android foreground and monochrome, 0.30
   splash; a square side of `reach × canvas / 0.678` for the LE-03 corner radius), centred on a
   1024 transparent canvas. The Icon Composer layer is the mark at 744 px, centred.
3. Take `icon.png` and `favicon.png` from the pack's colored-dark `ios/` and `web/` files
   (flatten to RGB, no alpha); the Android background is a solid `#0B0B18`.

Check sizes and alpha after a rebuild with `sips -g pixelWidth -g pixelHeight -g hasAlpha`, and
the reaches with a script that measures the alpha > 16 pixels.

## Verifying the splash on a simulator

iOS caches each app's launch screen per bundle version, and that cache survives an
uninstall, a reinstall and a simulator reboot. A build that changes only the splash can
keep showing the old one. To see the new splash, bump `CFBundleVersion` of the installed
build (for example `plutil -replace CFBundleVersion -string 2 EliteaAI.app/Info.plist`,
then `codesign -f -s - EliteaAI.app`) and install that. A store build needs nothing:
every release gets a new build number.
