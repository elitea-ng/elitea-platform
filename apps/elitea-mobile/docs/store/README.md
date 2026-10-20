# Store submission material

Everything the App Store and Google Play ask for, drafted from the code. The
release mechanics (accounts, EAS, TestFlight, OTA, rollback) are in
[docs/release.md](../release.md).

| File | What | Goes to |
|---|---|---|
| [listing.md](listing.md) | name, subtitle, descriptions, keywords, categories, age rating answers, review notes and the demo account the owner must create | App Store Connect, Play Console |
| [screenshots.md](screenshots.md) | sizes and the frame plan | both stores |
| [privacy.md](privacy.md) | the app's data flows, App Store privacy labels, Play Data safety, the iOS privacy manifest audit | App Privacy, Data safety, `app.config.ts` |
| [privacy-policy.md](privacy-policy.md) | privacy policy draft | `https://agent.zefir.ca/privacy` |
| [support.md](support.md) | support page draft | `https://agent.zefir.ca/support` |
| [export-compliance.md](export-compliance.md) | why `ITSAppUsesNonExemptEncryption` is `NO` | Info.plist, App Store Connect |

Values in `[brackets]` are the owner's to fill in. Decisions that are the
owner's, not the code's, are listed in docs/release.md §13.
