# Elitea AI assets

The app's fixed identity is the Elitea logo mark, the same artwork as the web app and the
desktop app (`apps/elitea-desktop/src-tauri/icons/`); its canonical source is
`libs/ts/client-core/src/brand/logo-mark.svg`. Rules for using it are in `docs/design-system.md` §5.

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

| File | Pixels / alpha | Key |
|---|---|---|
| `assets/elitea.icon/` (`icon.json` + `Assets/elitea.png`) | Icon Composer document | `ios.icon`. The mark (reach 0.40) on a light gradient fill (`#FFFFFF` → `#E8ECF2`) with a dark `fill-specializations` gradient (`#181F2A` → `#0E131D`); the system derives Dark, Tinted and Clear. Needs Xcode 26+. |
| `assets/images/icon.png` | 1024, opaque | `icon` (Android legacy, Expo fallback, store listing source): the mark (reach 0.40) on the light gradient. |
| `assets/images/android-icon-foreground.png` | 1024 (= 108 dp), alpha | `android.adaptiveIcon.foregroundImage`. The mark, every pixel within 0.29 of the canvas from the centre (66 dp safe circle = 0.306). |
| `assets/images/android-icon-background.png` | 1024, opaque | `android.adaptiveIcon.backgroundImage`, the light gradient; `backgroundColor: '#F4F6F9'`. |
| `assets/images/android-icon-monochrome.png` | 1024, alpha only | `android.adaptiveIcon.monochromeImage` (Android 13+ themed icons): the mark's silhouette, reach 0.25. |
| `assets/images/favicon.png` | 48, opaque | `web.favicon` |

There is one icon. The app had an alternate "workspace" icon under its former mascot branding;
it was removed together with that branding.

## Splash

| File | Pixels | Key |
|---|---|---|
| `assets/images/splash-icon.png` | 1024, alpha | `expo-splash-screen` `image` and `dark.image`. The mark, every pixel within 0.30 of the canvas (Android 12+ masks to a circle of radius 1/3). |

Backgrounds: light `#FBF7F9`, dark `#141118` (the `background` tokens), so the splash hands
over to the first screen without a flash.

## Regenerate

The rasters are made from `libs/ts/client-core/src/brand/logo-mark.svg`:

1. Render the SVG with transparency at 2048 px wide, e.g.
   `inkscape logo-mark.svg --export-type=png --export-width=2048 --export-filename=mark.png`.
2. Crop to the visible pixels and scale so the farthest visible pixel (alpha > 16) from the
   centre is the stated reach of the canvas (0.40 iOS layer and `icon.png`, 0.29 Android
   foreground, 0.25 monochrome, 0.30 splash), centred on a 1024 canvas.
3. Composite onto the gradients above where the file is opaque; the monochrome layer is the
   alpha channel in white.

Check the reaches after a rebuild, e.g. with a script that measures the alpha > 16 pixels.

## Verifying the splash on a simulator

iOS caches each app's launch screen per bundle version, and that cache survives an
uninstall, a reinstall and a simulator reboot. A build that changes only the splash can
keep showing the old one. To see the new splash, bump `CFBundleVersion` of the installed
build (for example `plutil -replace CFBundleVersion -string 2 EliteaAI.app/Info.plist`,
then `codesign -f -s - EliteaAI.app`) and install that. A store build needs nothing:
every release gets a new build number.
