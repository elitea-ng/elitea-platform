# Store paperwork for another brand

The files one level up are **ELITEA's** answers (brand/elitea). This folder is
the same set for any other brand (ADR-0052 decision 10), with placeholders:

- A name in braces is a value from your brand's `brand.json` (`{productName}`,
  `{mobile.bundleId}`, `{privacyUrl}`, ...). Copy it exactly.
- `[bracketed]` is yours to decide: your legal entity, contacts, the demo
  workspace you run for reviewers.

| File | Goes to |
|---|---|
| [listing.md](listing.md) | App Store Connect and Play Console: names, descriptions, ratings, review notes |
| [privacy-policy.md](privacy-policy.md) | your `{privacyUrl}` |
| [support.md](support.md) | your `{supportUrl}` |

Two answers are the same for every brand, because every brand builds the same
code (no code branches on a brand): the **privacy labels and Data safety form**
([../privacy.md](../privacy.md)) and **export compliance**
([../export-compliance.md](../export-compliance.md)). Use them as they are,
with your product name, and choose answer A or B in privacy.md for your own
situation. Re-check both against the release you build.

The screenshot plan ([../screenshots.md](../screenshots.md)) also applies; your
frames show your brand and your demo workspace.

The partner guide is "Build your own brand" (ELITEA docs, Development).
