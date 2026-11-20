---
date: 2026-11-13
order: 4
---
## Artifacts: rename a file, and why an upload failed

Files in an Artifacts bucket now have a **Rename** action, on the row and in the toolbar for one selected file. The rename is done by the server (copy, then delete), keeps the file in its folder, and refuses a name that is already taken. An upload that fails now says why: when a proxy or firewall in front of Elitea answers with a block page, the message says the upload was blocked before it reached Elitea; any other refusal says the server refused the upload, with its HTTP status, per file. Rename is offered only to people who may both create and delete files, and a renamed file keeps its retention. **Upgrade Main and the web app together.** See [Upload, preview, and manage files](/menus/artifacts#upload-preview-and-manage-files).
