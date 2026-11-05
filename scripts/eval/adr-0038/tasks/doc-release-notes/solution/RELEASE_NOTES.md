# quill v2.8.0

## Breaking changes

- **`--config` is now `--config-file`.** Update scripts and CI jobs that pass
  `--config`; the short form `-c` still works. (#436)
- **Node.js 22 or later is required.** Node 20 is no longer supported; upgrade
  your runtime before installing this release. (#446)
- **`quill build` exits non-zero when any file fails to render.** Previously it
  exited 0 if at least one file rendered. Scripts that relied on exit code 0 for
  partial failures must now handle exit code 2. (#452)

## Features

- PDF export: `quill build --format pdf` renders each page to PDF using the
  bundled print stylesheet. (#431)
- Compiled templates are cached between builds, making incremental builds much
  faster (4.1 s to 1.3 s on the docs site). (#443)
- Custom fonts can be set with the `fonts:` theme key. (#450)

## Fixes

- CRLF line endings in front matter are handled correctly. (#433)
- HTML in code block titles is now escaped, so a crafted title can no longer
  inject markup into the page. (#438)
- File events are debounced on macOS, so saving a file no longer triggers
  several rebuilds. (#445)
- markdown-it is updated to 14.1.1 to fix CVE-2026-40021, a denial-of-service
  issue with crafted link references. (#453)
