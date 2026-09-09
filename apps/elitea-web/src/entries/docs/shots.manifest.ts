/**
 * Screenshot manifest (PREAMBLE decision 4). One entry per `<Screenshot id>`
 * used anywhere under `content/`; `docs-content.test.ts` asserts every
 * `Screenshot` usage has an entry here, and (once the writers/tour capture
 * pass runs `apps/elitea-web/scripts/docs-shots.ts`) that the matching
 * `content/img/<id>.webp` exists and is ≤250 KB.
 *
 * No images are committed yet — this unit only wires the manifest shape and
 * a few entries pointing at real app routes, so `docs-shots.ts` (a later
 * unit) has a real, typed target list to capture against instead of an
 * empty file. `route` is the NEW app's route (this repo's truth source per
 * the PREAMBLE's content rules), not a legacy Mintlify path.
 *
 * The `shots` array below is hand-authored AND machine-regenerated: content
 * writers drop `shots.<batch>.json` files (arrays of the same shape, plus
 * `persona`/`notes`/`theme`) into a staging directory, and
 * `scripts/docs-shots-merge.mjs <dir>` folds them in here, deduping by `id`.
 * Editing an entry by hand is fine; running the merge again afterwards will
 * only complain if a staging file still disagrees with your edit.
 */

export interface ShotAction {
  /** `'click' | 'hover' | 'fill' | 'type' | 'press' | 'wait-for'` kept as a
   * free string: the capture script (docs-shots.ts, a Playwright driver) is
   * the one place that interprets it, and adding an action kind should not
   * require touching this type. */
  readonly type: string;
  readonly selector: string;
  readonly value?: string;
}

/**
 * The three storageState personas `playwright.config.ts` maintains
 * (`STORAGE_STATE.member` / `.admin` / `.chat`). `docs-shots.ts` signs in as
 * whichever one a shot names, falling back to its own `--persona` flag
 * (default `member`) when a shot omits this field.
 */
export type ShotPersona = "member" | "admin" | "chat";

export interface Shot {
  readonly id: string;
  /** Root-relative path into the running app, e.g. `/app/agents-hub`. */
  readonly route: string;
  readonly viewport: { readonly width: number; readonly height: number };
  /** Element to screenshot; the full viewport when absent. */
  readonly selector?: string;
  readonly actions?: readonly ShotAction[];
  /** CSS selectors to blank out before capture (timestamps, avatars, …). */
  readonly mask?: readonly string[];
  /** Which signed-in persona to capture the route as. Default: `member`. */
  readonly persona?: ShotPersona;
  /** Colour scheme to switch to (through the real toggle) before navigating.
   * Default: `light` — the PREAMBLE's decision 4 requires every committed
   * screenshot to be light-theme. */
  readonly theme?: "light" | "dark";
  /** Free-text authoring note (why this shot, what it should show). Never
   * read by `docs-shots.ts`; carried through so a writer's staging JSON and
   * the merged manifest say the same thing. */
  readonly notes?: string;
}

const DEFAULT_VIEWPORT = { width: 1440, height: 900 };

export const shots: readonly Shot[] = [
  { id: "chat-home", route: "/app/", viewport: DEFAULT_VIEWPORT },
  { id: "agents-hub", route: "/app/agents-hub", viewport: DEFAULT_VIEWPORT },
  {
    id: "elitea-catalog",
    route: "/app/elitea-catalog",
    viewport: DEFAULT_VIEWPORT,
  },
  {
    id: "chat-quick-start-composer",
    route: "/chat",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Empty-state composer with model selector visible; open the + menu for the shot.",
  },
  {
    id: "chat-quick-start-participants",
    route: "/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Seed autotest_docs_conversation with autotest_docs_agent attached as a participant.",
  },
  {
    id: "connect-toolkits-type-grid",
    route: "/toolkits/create",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Type tile grid, no seed beyond base.",
  },
  {
    id: "configure-ai-provider-form",
    route: "/settings/create-configuration/open_ai",
    viewport: DEFAULT_VIEWPORT,
    mask: ["api-key-field"],
    persona: "member",
    notes: "Mask the API key input.",
  },
  {
    id: "create-personal-token-generate",
    route: "/settings/create-personal-token",
    viewport: DEFAULT_VIEWPORT,
    mask: ["generated-token-value"],
    persona: "member",
    notes: "Capture the generated-token dialog; mask the token value.",
  },
  {
    id: "create-secret-dialog",
    route: "/settings/secrets",
    viewport: DEFAULT_VIEWPORT,
    mask: ["secret-value-field"],
    persona: "member",
    notes: "Open the create-secret dialog.",
  },
  {
    id: "create-credential-form",
    route: "/credentials/create-credential/jira",
    viewport: DEFAULT_VIEWPORT,
    mask: ["secret-fields"],
    persona: "member",
    notes: "Jira credential form as a representative example.",
  },
  {
    id: "create-artifact-bucket",
    route: "/artifacts/create-bucket",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Create-bucket dialog; a second crop of /artifacts file list is acceptable if the manifest needs two shots split into one.",
  },
  {
    id: "chat-conversation-list-folders",
    route: "/chat",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Seed one folder and one pinned conversation beyond base seed.",
  },
  {
    id: "chat-conversation-view",
    route: "/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    mask: ["assistant-message-text"],
    persona: "member",
    notes:
      "autotest_docs_conversation with 2-3 turns; mask assistant text unless the real-model lane is used.",
  },
  {
    id: "chat-plus-menu-open",
    route: "/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Open the composer + menu so Modules/Agents/Pipelines/Toolkits/MCPs categories are visible.",
  },
  {
    id: "attach-files-composer",
    route: "/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Attach a small file via page.setInputFiles to show the attachment chip before sending.",
  },
  {
    id: "chat-canvas-code",
    route: "/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Conversation containing a code-block message; open canvas on it.",
  },
  {
    id: "chat-agent-inline-editor",
    route: "/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "autotest_docs_agent as an existing participant; open its inline editor panel from chat.",
  },
  {
    id: "agent-tools-menu-mcp-toggle",
    route: "/agents/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "autotest_docs_agent Tools menu open, Elitea MCP Tools row visible.",
  },
  {
    id: "chat-add-participant-dialog",
    route: "/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Add-participant dialog with the admin persona available to add.",
  },
  {
    id: "user-public-agents-list",
    route: "/user-public/agents",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "One published agent visible with a Public badge.",
  },
  {
    id: "chat-context-budget-widget",
    route: "/settings/memory",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Context-window management settings; expanded view.",
  },
  {
    id: "chat-hitl-authorization-card",
    route: "/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Requires admin to set one github tool as sensitive first; scripted tool call or real-model lane to trigger the confirm card.",
  },
  {
    id: "indexes-tab-empty",
    route: "/toolkits/all/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "autotest_docs_github toolkit, Indexes tab, no index created yet.",
  },
  {
    id: "index-create-form",
    route: "/toolkits/all/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Indexes tab create-index dialog; needs pgvector + embedding configurations seeded.",
  },
  {
    id: "index-details-panel",
    route: "/toolkits/all/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Index detail view with configuration and reindex/delete actions.",
  },
  {
    id: "index-history-list",
    route: "/toolkits/all/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Index history list; needs at least one prior indexing run.",
  },
  {
    id: "toolkit-index-search-tools",
    route: "/toolkits/all/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Tools list showing index_data/search_data toggles.",
  },
  {
    id: "index-schedule-form",
    route: "/toolkits/all/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Indexes tab schedule form with a cron expression.",
  },
  {
    id: "index-source-create-form",
    route: "/toolkits/create/confluence",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Representative create form for one index-capable source; swap route per source if multiple shots are wanted.",
  },
  {
    id: "build-agent-with-ai-dialog",
    route: "/agents/create",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Build with AI dialog before submit (real answer needs the real-model lane).",
  },
  {
    id: "agent-save-as-version-dialog",
    route: "/agents/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "autotest_docs_agent (createAgentWithVersion fixture), Save As Version dialog open.",
  },
  {
    id: "fork-agent-dialog",
    route: "/agents/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Fork action from the agent info panel, target-project selector visible.",
  },
  {
    id: "entity-export-menu",
    route: "/agents/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Export option in the three-dot menu.",
  },
  {
    id: "entity-import-dialog",
    route: "/agents/latest",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Import dialog with an entity summary from a sample export file.",
  },
  {
    id: "agent-publish-dialog",
    route: "/agents/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Publish version dialog; admin agent_publishing section enabled.",
  },
  {
    id: "pipeline-flow-overview",
    route: "/pipelines/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "autotest_docs_pipeline (llm -> decision -> printer), entry point marked.",
  },
  {
    id: "pipeline-add-node-menu",
    route: "/pipelines/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Add-node menu open, showing only the nine runtime-admitted types.",
  },
  {
    id: "pipeline-yaml-editor",
    route: "/pipelines/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "YAML/editor panel tab.",
  },
  {
    id: "pipeline-run-state-dialog",
    route: "/pipelines/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    mask: ["run-output-text"],
    persona: "member",
    notes:
      "Run state dialog; needs a run from the real-model lane or a scripted trigger.",
  },
  {
    id: "pipeline-structure-states-sidebar",
    route: "/pipelines/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Flow editor States sidebar open, input/messages toggles visible.",
  },
  {
    id: "pipeline-nodes-add-menu",
    route: "/pipelines/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Same Add-node menu shot as pipeline-add-node-menu; kept separate id per its own page.",
  },
  {
    id: "pipeline-hitl-node",
    route: "/pipelines/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Pipeline containing a HITL node with Approve/Edit/Reject routes configured; select the node to show its config panel.",
  },
  {
    id: "pipeline-toolkit-node",
    route: "/pipelines/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Pipeline containing a Toolkit node; select it to show input-mapping fields.",
  },
  {
    id: "pipeline-decision-node",
    route: "/pipelines/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Pipeline containing a Decision node; select it to show the description and named outputs.",
  },
  {
    id: "pipeline-trigger-schedule-panel",
    route: "/pipelines/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    mask: ["trigger-url"],
    persona: "member",
    notes: "Triggers & schedules panel; mask the secret webhook URL.",
  },
  {
    id: "credential-select-dropdown",
    route: "/toolkits/create/jira",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Credential select dropdown in the toolkit creation form.",
  },
  {
    id: "jira-toolkit-create-form",
    route: "/toolkits/create/jira",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Jira create-toolkit form, first screen.",
  },
  {
    id: "test-tools-pane-result",
    route: "/toolkits/all/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    mask: ["error-body"],
    persona: "member",
    notes:
      "autotest_docs_openapi Test tools pane; an unroutable target gives an acceptable error result, mask the error body.",
  },
  {
    id: "chat-sidebar-folder",
    route: "/chat",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Sidebar folder containing conversations, from the organizing-entities seed.",
  },
  {
    id: "entities-pin-action",
    route: "/agents/latest",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Entity list row with the pin action highlighted.",
  },
  {
    id: "entities-search-and-tags",
    route: "/agents/latest",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Search box open, tag chips visible from the docs seed's two tags.",
  },
  {
    id: "chat-share-link-dialog",
    route: "/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Share dialog open on autotest_docs_conversation. Section fragment for menus/chat.",
  },
  {
    id: "chat-voice-button-and-config",
    route: "/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Voice button and config dialog; requires admin voice_features enabled and a tts configuration. Section fragment for menus/chat.",
  },
  {
    id: "artifacts-bucket-file-list",
    route: "/artifacts",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "autotest_docs_bucket with two uploaded files. Section fragment for menus/artifacts.",
  },
  {
    id: "skills-editor-test-panel",
    route: "/skills/all/:skillId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "autotest_docs_skill, test panel open. Section fragment for menus/skills.",
  },
  {
    id: "agent-evaluation-runs",
    route: "/agents/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    mask: ["run-scores"],
    persona: "member",
    notes:
      "Evaluation tab, Runs sub-view with a scorecard; needs one eval dimension and one run seeded. Section fragment for menus/agents.",
  },
];
