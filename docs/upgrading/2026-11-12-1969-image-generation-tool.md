# Agents: the Image creation module generates images (#1969)

**What changed.** The Image creation toggle in Modules (chat and agent Tools) now
runs on the Rust worker. The assistant gets an `image_generation` tool with a
prompt, a size, a count (1 to 4) and an optional quality. It saves the images as
artifacts in the conversation's `attachments` bucket and shows them in the chat.
The gateway bills each image to the project. Before, the toggle was hidden by a
retired plugin check, and the worker skipped the tool.

**Who is affected.** Every deployment. The toggle is disabled, with the note "No
image model is configured for this deployment", until you set the model.

**What to do.** Set `global.imageGenerationModel` to the registry name of an
`image_generation_model` that your LLM gateway serves. The chart passes the value
to elitea-main (`ELITEA_IMAGE_GENERATION_MODEL`) and to the worker
(`image_generation_model` in `runtime.json`), so the toggle and the worker agree.
The model must be a global shared model (a model of the public project with
`shared` on). The gateway looks the name up in the catalogue of the project that
makes the call, so a model that one project owns fails in the other projects.
Leave the value empty to keep image generation off.

**Limits.** The model is one per deployment. Choosing the project's default image
model is a follow-up. The desktop app's local agent loop does not offer the tool
yet.
