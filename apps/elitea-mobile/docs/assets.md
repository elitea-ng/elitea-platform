# Agent Zefirca assets

The mascot, app-icon and splash assets of Agent Zefir. Every file is generated from one
source render, `design/mascot-zefirca-original.jpeg` (1408x768), by the offline pipeline
in `design/tools/`. No generative model was used: the character is the original render,
cut out and resampled, so it is identical everywhere it appears.

Rules for using the mascot are in `docs/design-system.md` §5; the component is
`src/components/Mascot.tsx`.

## In the app

### Mascot (`assets/images/mascot/`), rendered by `<Mascot variant size />`

| File | Pixels | Variant | Use |
|---|---|---|---|
| `mascot-full.png`, `@2x`, `@3x` | 574x676 / 1148x1352 / 1722x2028 | `full` | Full body without the pedestal. Empty states, About, "no workspace". Sits on any themed surface. |
| `mascot-hero.png`, `@2x`, `@3x` | 655x770 / 1310x1540 / 1965x2310 | `hero` | Full body on the mint pedestal. The welcome / connect hero only. |
| `mascot-bust.png` | 1080x884 | `bust` | Head, hologram hand and coat to the belt, bottom faded. Headers, voice screen, lock / privacy cover. |
| `avatar-head.png` | 384x384 | `avatar` | Face-centred head, hologram removed. Drawn in a circle on Zefir's soft-rose disc (never the workspace accent). Agent-avatar fallback. |

Metro picks `@2x`/`@3x` by screen density. The bust and avatar ship one size each, so
they are sharp on a @3x screen only up to a maximum width: **avatar 128 pt, bust 360 pt**
(`MASCOT_MAX_SHARP_SIZE` in `Mascot.tsx`; full 574 pt, hero 655 pt). Every documented use
(28-96 pt) is well under it; a larger use needs a bigger file from the pipeline first.

### App icon: SaaS default set (wired in `app.config.ts`)

| File | Pixels / alpha | Key |
|---|---|---|
| `assets/zefirca.icon/` (`icon.json` + `Assets/zefirca.png`) | Icon Composer document | `ios.icon`. The head mark (reach 0.40) on a light gradient fill with a dark `fill-specializations` gradient; the system derives Dark, Tinted and Clear. Needs Xcode 26+. |
| `assets/images/icon.png` | 1024, opaque | `icon` (Android legacy, Expo fallback, store listing source): the head mark (reach 0.40) on the light gradient. |
| `assets/images/android-icon-foreground.png` | 1024 (= 108 dp), alpha | `android.adaptiveIcon.foregroundImage`. The head mark, every pixel within 0.29 of the canvas from the centre (66 dp safe circle = 0.306). |
| `assets/images/android-icon-background.png` | 1024, opaque | `android.adaptiveIcon.backgroundImage`, pink → lavender → mint; `backgroundColor: '#EDE4FA'`. |
| `assets/images/android-icon-monochrome.png` | 1024, alpha only | `android.adaptiveIcon.monochromeImage` (Android 13+ themed icons). A flat glyph: swirl-head outline, eyes, brows, smile and two swirl grooves cut out; reach 0.25. |
| `assets/images/favicon.png` | 48, opaque | `web.favicon` |

### App icon: workspace / self-hosted set ("Zefir Night")

The second, user-confirmed icon of the alternate-icon plan (`docs/design-system.md` §3.6). The
same head mark from `build.py` on a neutral slate gradient (light `#F4F6F9` → `#E6EAF0` →
`#D3DAE3`, dark `#3A4049` → `#262A31` → `#16191E`), held to the same reaches as the default set.
Wired by `expo-alternate-app-icons` in `app.config.ts` under the name `ZefirNight`.

| File | Pixels / alpha | Key |
|---|---|---|
| `assets/images/workspace-icon/ios-icon-light.png` | 1024, opaque | iOS `ZefirNight.appiconset`, default appearance |
| `assets/images/workspace-icon/ios-icon-dark.png` | 1024, opaque | dark appearance (the pipeline's `ios-icon-dark-opaque.png`: the dark slate gradient, not the transparent variant) |
| `assets/images/workspace-icon/ios-icon-tinted.png` | 1024, alpha | tinted (mono) appearance |
| `assets/images/workspace-icon/android-icon-foreground.png` | 1024 (= 108 dp), alpha | the alias's adaptive foreground (reach 0.29) |
| `assets/images/workspace-icon/android-icon-background.png` | 1024, opaque | the alias's adaptive background (slate gradient); `backgroundColor: '#E6EAF0'` |
| `assets/images/workspace-icon/android-icon-monochrome.png` | 1024, alpha only | the alias's monochrome layer (Android 13+ themed icons) |
| `assets/zefirca-workspace.icon/` | Icon Composer document | NOT wired: kept as the source for when alternate icons take `.icon` documents (below) |

**Icon Composer and alternate icons.** The primary icon is an Icon Composer `.icon` document
(`ios.icon`). The alternate is an asset-catalog `appiconset` with light, dark and tinted
appearances, named in `ASSETCATALOG_COMPILER_ALTERNATE_APPICON_NAMES` (Xcode then writes
`CFBundleAlternateIcons`), because `expo-alternate-app-icons` writes asset catalogs and the
audit's caution stands: an alternate given as a `.icon` document needs Xcode 26's own alternate
handling, which this toolchain does not drive. The visible difference is the Clear appearance:
iOS 26 derives it for the `.icon` primary, the alternate shows its tinted art there. Switch the
alternate to `zefirca-workspace.icon` once the plugin (or our own) can add a `.icon` alternate.

**How it is offered.** Once per device, when a self-hosted workspace (`deployment_kind:
self_hosted`) is connected (src/features/appIcon/appIcon.ts); iOS then shows its own "You have
changed the icon" alert. Settings → Appearance → "Workspace app icon" switches either way.
Android: the icon is an `<activity-alias>` (`.MainActivityZefirNight`) enabled instead of
`.MainActivity`; `plugins/withAlternateIconLinks.js` copies MainActivity's link filters onto it so
connect links and the sign-in redirect keep working with that icon on, and some launchers close
the app during the switch (the offer says so).

### Splash

| File | Pixels | Key |
|---|---|---|
| `assets/images/splash-mascot.png` | 1024, alpha | `expo-splash-screen` `image` and `dark.image`. The bust with the hologram, every pixel within 0.30 of the canvas (Android 12+ masks to a circle of radius 1/3). |

Backgrounds: light `#FBF7F9`, dark `#141118` (the `background` tokens), so the splash hands
over to the first screen without a flash.

## Palette used by the icons

| | Light | Dark |
|---|---|---|
| Icon backdrop (default) | `#FCE4F1` → `#EDE4FA` → `#D5F2E9` | `#3A2A4F` → `#241D36` → `#12202A` |

## Decisions and limits

- **Pedestal:** the UI uses the figure without it (`full`); the pedestal is for the hero only.
  Icons and the splash never include it.
- **Hologram:** kept in the full body, bust, icons and splash; removed from the avatar, where
  it would be clipped. The source renders it as translucent glass with the background baked
  in, so on dark surfaces it reads as a pale glowing disc (intended).
- **Resolution:** the character covers about 590x710 px of the source, so the 3x files and the
  1024 icons are upsampled 1.2x to 3x (premultiplied Lanczos + light unsharp mask). They hold
  up at phone sizes and are slightly soft at 1024. For a sharper master, re-render the
  character at 2048+ px, replace `design/mascot-zefirca-original.jpeg`, adjust the crop boxes
  in `design/tools/build.py` (source pixel coordinates) and re-run.
- **Icon mark:** every icon is one head-only mark (head, headset and antenna; the collar fades
  out below the chin), centred, never the full body or the bust. `build.py` states each
  output's reach (the farthest visible pixel from the centre, as a fraction of the canvas):
  iOS layer 0.40 (clear of the squircle corners), Android foreground 0.29 (66 dp safe circle
  0.306), monochrome 0.25, splash 0.30 (Android 12+ circle 0.333). Check them after a
  rebuild, e.g. with a script that measures the alpha > 16 pixels.
- **Expressions:** one (neutral, happy). Until an artist supplies "thinking" and "sorry" poses,
  show those states as overlays on the same avatar (animated dots / pulsing ring for thinking,
  a small warning badge for errors).
- **Branding:** icons are fixed binaries (OS rule); a brand pack never changes them. Brand packs
  change in-app surfaces only, and never recolour the mascot.

## Regenerate

```sh
design/tools/run.sh "$PWD/design"     # writes design/work/ and design/assets/ (git-ignored)
```

Then copy the files you ship from `design/assets/` to the paths above (`icons/*.png` →
`assets/images/`, `icons/zefirca.icon/` → `assets/zefirca.icon/`, `splash/splash-mascot.png` →
`assets/images/`; the mascot files are renamed: `mascot-full.png` → `@3x`, `mascot-full@1x.png` → 1x; `mascot-full-pedestal*` →
`mascot-hero*`; the avatars and bust are downscaled with `sips -Z`).

Stages:

1. `cutout.swift` (compiled on first run): macOS Vision `VNGenerateForegroundInstanceMaskRequest`.
2. `cut.py`: tightens the mask, decontaminates edges, removes the pedestal (a geometric
   cylinder plus a mint colour test that keeps the feet).
3. `build.py`: every asset, including the `.icon` documents.
4. `ictool` (Xcode's Icon Composer) renders the `.icon` appearances for review.
5. `preview.py`: a contact sheet, `design/assets/preview.png`.

Requirements: macOS 14+, Swift, Xcode 26+ (for `ictool`), `python3` with Pillow, numpy, scipy.

## Verifying the splash on a simulator

iOS caches each app's launch screen per bundle version, and that cache survives an
uninstall, a reinstall and a simulator reboot. A build that changes only the splash can
keep showing the old one (on this project: the template's blue Expo chevron). To see
the new splash, bump `CFBundleVersion` of the installed build (for example
`plutil -replace CFBundleVersion -string 2 AgentZefir.app/Info.plist`, then
`codesign -f -s - AgentZefir.app`) and install that. A store build needs nothing:
every release gets a new build number.
