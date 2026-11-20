---
date: 2026-11-09
order: 2
---
## Agent avatar packs

An agent version can now have its own avatar for each state: a still, and an image, an animated WebP, or a muted MP4 or WebM loop for idle, thinking, speaking, working, waiting, success, error and waking. Set them on the agent editor's new **Avatar** tab, which previews every state before you save. The server checks every file: at most 4 seconds, 512 px and 2 MiB, no sound, and an SVG with no script. With an image generation model, **Generate** makes still candidates from the persona and loops from the still. A state with no file keeps the default motion. **Run the migrations.** See [Avatar](/menus/agents#avatar).
