---
title: "New image: elitea-browser-session (#1594, ADR-0049 decisions 2 and 4)"
issue: 1594
date: 2026-11-05
adr: ADR-0049
order: 76
---

Releases now publish `elitea-browser-session`, the cloud browser's
session image. It holds the in-pod agent at
`/usr/local/bin/elitea-browser-session` and chrome-headless-shell from
Chrome for Testing 157.0.8103.38, verified by checksum, on Debian 13.

Nothing runs it yet: the sandbox supervisor's browser sessions (#1595)
and the Helm wiring start it in later releases. No migration and no
configuration change.

If you mirror release images into a private registry, add
`elitea-browser-session` to the list so the browser sessions can pull it
when they land. It is built for `linux/amd64` and `linux/arm64`.

The release scan reports this image but does not block on its findings,
as with `elitea-workspace-runner`. Most of what it finds is in the Debian
libraries the engine links (`.github/image-scan-exempt.txt`).
