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
  /** Free-text description of the fixture this shot needs beyond the base
   * e2e seed (e.g. `"autotest_docs_agent"`) — documentation only, matched
   * against `docs-seed.ts`'s actual output by eye, not read by any script. */
  readonly seed?: string;
  /**
   * Overrides which `docs-seed.json` key resolves a route placeholder, for
   * the one case a name-based default cannot express: several shots share
   * the `:toolkitId` placeholder but want DIFFERENT seeded toolkits (most
   * want the github one; `test-tools-pane-result` wants the openapi one).
   * `docs-shots.ts` resolves `:name` from `seedMap[name]` by default (so
   * `:agentId` -> `seedMap.agentId`, `:pipelineId` -> `seedMap.pipelineId`,
   * `:toolkitId` -> `seedMap.toolkitId`, the github toolkit); a key present
   * here for the same placeholder name is used instead, e.g.
   * `{ toolkitId: 'openapiToolkitId' }`.
   */
  readonly placeholders?: Readonly<Record<string, string>>;
}

const DEFAULT_VIEWPORT = { width: 1440, height: 900 };

export const shots: readonly Shot[] = [
  {
    id: "chat-quick-start-composer",
    route: "/app/chat",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Empty-state composer with model selector visible; open the + menu for the shot.",
  },
  {
    id: "chat-quick-start-participants",
    route: "/app/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Seed autotest_docs_conversation with autotest_docs_agent attached as a participant.",
  },
  {
    id: "connect-toolkits-type-grid",
    route: "/app/toolkits/create",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Type tile grid, no seed beyond base.",
  },
  {
    id: "configure-ai-provider-form",
    route: "/app/settings/create-configuration/open_ai",
    viewport: DEFAULT_VIEWPORT,
    mask: ["api-key-field"],
    persona: "member",
    notes: "Mask the API key input.",
  },
  {
    id: "create-personal-token-generate",
    route: "/app/settings/create-personal-token",
    viewport: DEFAULT_VIEWPORT,
    // "Generate" is disabled until Name is filled (CreatePersonalToken.tsx has
    // no data-testid, so this fills by the field's own aria-label).
    actions: [
      { type: "fill", selector: "[aria-label='Name']", value: "autotest_docs_token_shot" },
      { type: "click", selector: "text=Generate" },
    ],
    mask: ["[data-testid='generated-token-value']"],
    persona: "member",
    notes: "Capture the generated-token dialog; mask the token value.",
  },
  {
    id: "create-secret-dialog",
    // `?createSecret=1` opens the dialog directly by URL (Secrets.tsx,
    // per e2e/journeys/settings/settings.secrets.spec.ts's own J21d test) —
    // reused here instead of hunting for a click target.
    route: "/app/settings/secrets?createSecret=1",
    viewport: DEFAULT_VIEWPORT,
    mask: ["secret-value-field"],
    persona: "member",
    notes: "Open the create-secret dialog.",
  },
  {
    id: "create-credential-form",
    route: "/app/credentials/create-credential/jira",
    viewport: DEFAULT_VIEWPORT,
    // Unlike the TOOLKIT create route (see toolkit-new-generic etc.), the
    // credentials create page's parent component DOES read `:credentialType`
    // directly off the URL — confirmed live, the Jira form renders with no
    // click needed.
    mask: ["secret-fields"],
    persona: "member",
    notes: "Jira credential form as a representative example.",
  },
  {
    id: "create-artifact-bucket",
    route: "/app/artifacts/create-bucket",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Create-bucket dialog; a second crop of /artifacts file list is acceptable if the manifest needs two shots split into one.",
  },
  {
    id: "chat-conversation-list-folders",
    route: "/app/chat",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Seed one folder and one pinned conversation beyond base seed.",
  },
  {
    id: "chat-conversation-view",
    route: "/app/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    mask: ["assistant-message-text"],
    persona: "member",
    notes:
      "autotest_docs_conversation with 2-3 turns; mask assistant text unless the real-model lane is used.",
  },
  {
    id: "chat-plus-menu-open",
    route: "/app/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    actions: [{ type: "click", selector: "[data-testid='plus-menu-button']" }],
    persona: "member",
    notes:
      "Open the composer + menu so Modules/Agents/Pipelines/Toolkits/MCPs categories are visible.",
  },
  {
    id: "attach-files-composer",
    route: "/app/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Attach a small file via page.setInputFiles to show the attachment chip before sending.",
  },
  {
    id: "chat-canvas-code",
    route: "/app/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Conversation containing a code-block message; open canvas on it.",
  },
  {
    id: "chat-agent-inline-editor",
    route: "/app/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "autotest_docs_agent as an existing participant; open its inline editor panel from chat.",
  },
  {
    id: "agent-tools-menu-mcp-toggle",
    route: "/app/agents/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    selector: "[data-testid='agent-toolkits-section']",
    persona: "member",
    notes:
      "autotest_docs_agent Tools section (ApplicationTools.tsx), cropped to the " +
      "agent-toolkits-section accordion: attached toolkit card plus the INTERNAL " +
      "TOOLS grid, where the 'Elitea MCP Tools' switch lives. A live dropdown-menu " +
      "capture (the '+ Tool' picker actually open) was tried and dropped as too " +
      "fragile (MUI popper positioning); the static section reads the same fact.",
  },
  {
    id: "chat-add-participant-dialog",
    route: "/app/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Add-participant dialog with the admin persona available to add.",
  },
  {
    id: "user-public-agents-list",
    route: "/app/user-public/agents",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "One published agent visible with a Public badge.",
  },
  {
    id: "chat-context-budget-widget",
    route: "/app/settings/memory",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Context-window management settings; expanded view.",
  },
  {
    id: "chat-hitl-authorization-card",
    route: "/app/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Requires admin to set one github tool as sensitive first; scripted tool call or real-model lane to trigger the confirm card.",
  },
  {
    id: "indexes-tab-empty",
    route: "/app/toolkits/all/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    // Every "Indexes tab" shot below was landing on the default Configuration
    // tab (confirmed live: five of these plus `mcp-tool-selection` were
    // byte-identical) — none had an action to click the tab. EditToolkit.tsx's
    // second `BaseTab` is literally labelled "Indexes".
    actions: [{ type: "click", selector: "role=tab[name='Indexes']" }],
    persona: "member",
    notes: "autotest_docs_github toolkit, Indexes tab, no index created yet.",
  },
  {
    id: "index-create-form",
    route: "/app/toolkits/all/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    actions: [{ type: "click", selector: "role=tab[name='Indexes']" }],
    persona: "member",
    notes:
      "Indexes tab create-index dialog; needs pgvector + embedding configurations seeded — " +
      "this docs seed does not open the dialog itself (no confirmed 'Add index' selector), so " +
      "this currently shows the Indexes tab's own empty/list state, not the dialog.",
  },
  {
    id: "index-details-panel",
    route: "/app/toolkits/all/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    actions: [{ type: "click", selector: "role=tab[name='Indexes']" }],
    persona: "member",
    notes:
      "Index detail view with configuration and reindex/delete actions — needs an actual " +
      "index run, which this stack cannot produce (no worker/index execution plane; see " +
      "docs-seed.ts's module doc). Currently shows the same empty Indexes tab.",
  },
  {
    id: "index-history-list",
    route: "/app/toolkits/all/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    actions: [{ type: "click", selector: "role=tab[name='Indexes']" }],
    persona: "member",
    notes:
      "Index history list; needs at least one prior indexing run, which this stack cannot " +
      "produce. Currently shows the same empty Indexes tab.",
  },
  {
    id: "toolkit-index-search-tools",
    route: "/app/toolkits/all/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Tools list showing index_data/search_data toggles — this is the CONFIGURATION tab " +
      "(not Indexes): the toggles are two entries in the toolkit's own selected_tools list, " +
      "which docs-seed.ts populates for github when this deployment's catalogue offers them.",
  },
  {
    id: "index-schedule-form",
    route: "/app/toolkits/all/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    actions: [{ type: "click", selector: "role=tab[name='Indexes']" }],
    persona: "member",
    notes:
      "Indexes tab schedule form with a cron expression — needs an actual index to attach a " +
      "schedule to, which this stack cannot produce. Currently shows the same empty Indexes tab.",
  },
  {
    id: "index-source-create-form",
    route: "/app/toolkits/create/confluence",
    viewport: DEFAULT_VIEWPORT,
    // `create.$toolkitType.tsx` is an empty child route — the URL segment is
    // never read; the type is selected by clicking its tile (confirmed live).
    actions: [{ type: "click", selector: "text=Confluence" }],
    persona: "member",
    notes:
      "Representative create form for one index-capable source; swap route per source if multiple shots are wanted.",
  },
  {
    id: "build-agent-with-ai-dialog",
    route: "/app/agents/create",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Build with AI dialog before submit (real answer needs the real-model lane).",
  },
  {
    id: "agent-save-as-version-dialog",
    route: "/app/agents/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    actions: [{ type: "click", selector: "text=Save As Version" }],
    persona: "member",
    notes:
      "autotest_docs_agent (createAgentWithVersion fixture), Save As Version dialog open.",
  },
  {
    id: "fork-agent-dialog",
    route: "/app/agents/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    actions: [
      { type: "click", selector: "[data-testid='agent-lifecycle-menu-button']" },
      { type: "click", selector: "[data-testid='agent-fork-menuitem']" },
    ],
    persona: "member",
    notes:
      "Fork action from the agent's lifecycle menu (EntityLifecycleMenu.tsx), target-project selector visible.",
  },
  {
    id: "entity-export-menu",
    route: "/app/agents/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    actions: [{ type: "click", selector: "[data-testid='agent-lifecycle-menu-button']" }],
    persona: "member",
    notes: "Export option in the lifecycle menu (EntityLifecycleMenu.tsx).",
  },
  {
    id: "entity-import-dialog",
    route: "/app/agents/latest",
    viewport: DEFAULT_VIEWPORT,
    actions: [{ type: "click", selector: "[data-testid='agents-import-button']" }],
    persona: "member",
    notes:
      "Import dialog with an entity summary from a sample export file — docs-shots.ts's action " +
      "vocabulary has no file-upload step, so this shows the empty import dialog only, not a " +
      "parsed file summary.",
  },
  {
    id: "agent-publish-dialog",
    route: "/app/agents/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    actions: [
      { type: "click", selector: "[data-testid='agent-lifecycle-menu-button']" },
      { type: "click", selector: "[data-testid='agent-publish-menuitem']" },
    ],
    persona: "member",
    notes: "Publish version dialog; publishing_enabled is on by default (confirmed live).",
  },
  {
    id: "pipeline-flow-overview",
    route: "/app/pipelines/latest/:pipelineId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "autotest_docs_pipeline (llm -> decision -> printer), entry point marked.",
  },
  {
    id: "pipeline-add-node-menu",
    route: "/app/pipelines/latest/:pipelineId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Add-node menu open, showing only the nine runtime-admitted types.",
  },
  {
    id: "pipeline-yaml-editor",
    route: "/app/pipelines/latest/:pipelineId",
    viewport: DEFAULT_VIEWPORT,
    // EditorPanel.tsx's mode toggle is generated from `Object.entries(PipelineEditorMode)`
    // so its labels are the enum's own keys — "Flow" and "Yaml".
    actions: [{ type: "click", selector: "text=Yaml" }],
    persona: "member",
    notes: "YAML/editor panel tab.",
  },
  {
    id: "pipeline-run-state-dialog",
    route: "/app/pipelines/latest/:pipelineId",
    viewport: DEFAULT_VIEWPORT,
    mask: ["run-output-text"],
    persona: "member",
    notes:
      "Run state dialog; needs a run from the real-model lane or a scripted trigger.",
  },
  {
    id: "pipeline-structure-states-sidebar",
    route: "/app/pipelines/latest/:pipelineId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Flow editor States sidebar open, input/messages toggles visible.",
  },
  {
    id: "pipeline-nodes-add-menu",
    route: "/app/pipelines/latest/:pipelineId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Same Add-node menu shot as pipeline-add-node-menu; kept separate id per its own page.",
  },
  {
    id: "pipeline-hitl-node",
    route: "/app/pipelines/latest/:pipelineId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Pipeline containing a HITL node with Approve/Edit/Reject routes configured; select the node to show its config panel.",
  },
  {
    id: "pipeline-toolkit-node",
    route: "/app/pipelines/latest/:pipelineId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Pipeline containing a Toolkit node; select it to show input-mapping fields.",
  },
  {
    id: "pipeline-decision-node",
    route: "/app/pipelines/latest/:pipelineId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Pipeline containing a Decision node; select it to show the description and named outputs.",
  },
  {
    id: "pipeline-trigger-schedule-panel",
    route: "/app/pipelines/latest/:pipelineId",
    viewport: DEFAULT_VIEWPORT,
    mask: ["trigger-url"],
    persona: "member",
    notes: "Triggers & schedules panel; mask the secret webhook URL.",
  },
  {
    id: "credential-select-dropdown",
    route: "/app/toolkits/create/jira",
    viewport: DEFAULT_VIEWPORT,
    // `create.$toolkitType.tsx` is an empty child route (no component reads
    // the URL segment) — select the type by clicking its tile first, then
    // open its credential select.
    actions: [
      { type: "click", selector: "text=Jira" },
      { type: "click", selector: "text=Jira Configuration" },
    ],
    persona: "member",
    notes: "Credential select dropdown in the toolkit creation form.",
  },
  {
    id: "jira-toolkit-create-form",
    route: "/app/toolkits/create/jira",
    viewport: DEFAULT_VIEWPORT,
    actions: [{ type: "click", selector: "text=Jira" }],
    persona: "member",
    notes: "Jira create-toolkit form, first screen.",
  },
  {
    id: "test-tools-pane-result",
    route: "/app/toolkits/all/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    mask: ["error-body"],
    persona: "member",
    placeholders: { toolkitId: "openapiToolkitId" },
    notes:
      "autotest_docs_openapi Test tools pane; an unroutable target gives an acceptable error result, mask the error body.",
  },
  {
    id: "chat-sidebar-folder",
    route: "/app/chat",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Sidebar folder containing conversations, from the organizing-entities seed.",
  },
  {
    id: "entities-pin-action",
    route: "/app/agents/latest",
    viewport: DEFAULT_VIEWPORT,
    actions: [{ type: "hover", selector: "text=autotest_docs_agent" }],
    persona: "member",
    notes: "Entity list row with the pin action highlighted (hover reveals the row's actions).",
  },
  {
    id: "entities-search-and-tags",
    route: "/app/agents/latest",
    viewport: DEFAULT_VIEWPORT,
    actions: [{ type: "click", selector: "[data-testid='agent-search-input']" }],
    persona: "member",
    notes:
      "Search box focused; tag chips from the docs seed's two tags render as their numeric " +
      "ids on the card (a real product display gap — the card shows tag ids, not tag names), " +
      "not names.",
  },
  {
    id: "overview-sidebar",
    route: "/app/chat",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Sidebar with main menu items and project selector visible; no extra seed needed.",
  },
  {
    id: "chat-conversation",
    route: "/app/chat/:conversationId",
    viewport: DEFAULT_VIEWPORT,
    mask: ["assistant-message-text"],
    persona: "member",
    notes: "Mask assistant bubble text unless the real-model E2E lane is used.",
    seed: "autotest_docs_conversation",
  },
  {
    id: "agents-list",
    route: "/app/agents/latest",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    seed: "autotest_docs_agent",
  },
  {
    id: "pipelines-flow-editor",
    route: "/app/pipelines/latest/:pipelineId",
    viewport: DEFAULT_VIEWPORT,
    // `[data-testid='flow-editor']` does not exist anywhere in
    // features/pipelines — the flow canvas is `@xyflow/react`'s own
    // `<ReactFlow>`, whose root always carries the library's OWN `.react-flow`
    // class (confirmed live: the invented testid timed out, `.react-flow` did
    // not).
    selector: ".react-flow",
    persona: "member",
    seed: "autotest_docs_pipeline",
  },
  {
    id: "skills-list",
    route: "/app/skills/all",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Use a team or personal project — Skills is hidden in the Public project.",
    seed: "autotest_docs_skill",
  },
  {
    id: "toolkits-list",
    route: "/app/toolkits/all",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    seed: "autotest_docs_github",
  },
  {
    id: "mcps-list",
    route: "/app/mcps/all",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Requires admin flag mcp_in_menu_enabled turned on for the seeded project.",
  },
  {
    id: "apps-catalog",
    route: "/app/apps/catalog",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Needs one catalogue app with the request-access button (centry.moderation_state flow enabled).",
  },
  {
    id: "credentials-list",
    route: "/app/credentials/all",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    seed: "autotest_docs_github_cred",
  },
  {
    id: "artifacts-buckets",
    route: "/app/artifacts",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Requires ELITEA_ARTIFACTS_ENABLED.",
    seed: "autotest_docs_bucket + 2 files",
  },
  {
    id: "catalog-agents",
    route: "/app/elitea-catalog",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Needs one published agent (publish route router.go:3048).",
  },
  {
    id: "deepwiki-workspace",
    route: "/app/deepwiki/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Needs a wiki toolkit; generated pages need the DeepWiki real-engine compose overlay, else shoot the empty/picker state at /deepwiki.",
  },
  {
    id: "inventory-workspace",
    route: "/app/inventory/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Needs an inventory toolkit with at least one ingested source.",
  },
  {
    id: "help-center",
    route: "/app/help-center",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes: "Needs admin Configuration › Resources with 2+ links configured.",
  },
  {
    id: "support-assistant",
    route: "/app/chat",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Needs support_assistant section enabled with a support_project_id; open the widget before capturing.",
  },
  {
    id: "settings-drawer",
    route: "/app/settings/project-general",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
  },
  {
    id: "settings-general",
    route: "/app/settings/project-general",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
  },
  {
    id: "settings-secrets",
    route: "/app/settings/secrets",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    seed: "autotest_docs_secret",
  },
  {
    id: "settings-ai-providers",
    route: "/app/settings/model-configuration",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    seed: "ai_credentials fixture (.invalid base)",
  },
  {
    id: "settings-analytics",
    route: "/app/settings/analytics",
    viewport: DEFAULT_VIEWPORT,
    persona: "admin",
    notes:
      "Requires admin flag analytics_enabled and some llm_usage_events rows (real-model lane or gateway mock).",
  },
  {
    id: "settings-usage",
    route: "/app/settings/usage",
    viewport: DEFAULT_VIEWPORT,
    persona: "admin",
    notes: "Requires admin flag cost_budgets_enabled.",
  },
  {
    id: "settings-profile",
    route: "/app/settings/profile",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
  },
  {
    id: "settings-preferences",
    route: "/app/settings/preferences",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
  },
  {
    id: "settings-ai-personality",
    route: "/app/settings/ai-personality",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
  },
  {
    id: "settings-memory",
    route: "/app/settings/memory",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
  },
  {
    id: "settings-tokens",
    route: "/app/settings/tokens",
    viewport: DEFAULT_VIEWPORT,
    mask: ["[data-testid='generated-token-value']"],
    persona: "member",
    seed: "one token created by the docs seed",
  },
  {
    id: "settings-notifications",
    route: "/app/settings/notifications",
    viewport: DEFAULT_VIEWPORT,
    persona: "admin",
    notes:
      "Trigger one app-request decision as admin first so the list is non-empty.",
  },
  {
    id: "internal-tools-menu",
    route: "/app/agents/latest/:agentId",
    viewport: DEFAULT_VIEWPORT,
    selector: "[data-testid='agent-toolkits-section']",
    persona: "member",
    notes:
      "Open the Tools › Modules list; never shoot a module actually running unless the deployment's worker is Python. Same real container as agent-tools-menu-mcp-toggle (ApplicationTools.tsx's agent-toolkits-section accordion) — ToolMenu.tsx itself has no testid.",
    seed: "autotest_docs_agent",
  },
  {
    id: "toolkit-new-generic",
    route: "/app/toolkits/create/confluence",
    viewport: DEFAULT_VIEWPORT,
    // `create.$toolkitType.tsx` is an empty child route — select the type by
    // clicking its tile (confirmed live; the URL segment alone is a no-op).
    actions: [{ type: "click", selector: "text=Confluence" }],
    persona: "member",
    notes:
      "Generic saved-credential toolkit create form, captured once against Confluence and reused across every toolkit page whose form is just a category/label/credential-select (ado_boards, ado_plans, ado_repos, ado_wiki, aha, bitbucket, confluence, figma, github, gitlab, gitlab_org, google_places, jira, openapi, postman, powerpoint (pptx), qtest, rally, reportportal (report_portal), salesforce, servicenow (service_now), sharepoint, slack, sonar, sql, testIO (testio), testrail, xray (xray_cloud), zephyr_enterprise, zephyr_scale). Only the type label in the header differs between these; do not create a per-type capture for any of them.",
  },
  {
    id: "toolkit-new-artifact",
    route: "/app/toolkits/create/artifact",
    viewport: DEFAULT_VIEWPORT,
    actions: [{ type: "click", selector: "role=button[name='Artifact']" }],
    persona: "member",
    notes:
      "Built-in Artifact toolkit form differs from the generic saved-credential form (bucket picker, no vendor credential) — distinct capture from toolkit-new-generic.",
  },
  {
    id: "toolkit-new-memory",
    route: "/app/toolkits/create/memory",
    viewport: DEFAULT_VIEWPORT,
    actions: [{ type: "click", selector: "text=Memory" }],
    persona: "member",
    notes:
      "Built-in Memory toolkit form (pgvector credential only, no vendor auth) — distinct capture from toolkit-new-generic.",
  },
  {
    id: "toolkit-new-other-toolkits",
    route: "/app/toolkits/create",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Type picker tile grid, for the other-toolkits reference page. The batch-C staging entry named a 'filter category: cloud' action with no selector and no defined mechanism (not a valid ShotAction) — its own fallback clause applies: the unfiltered grid is captured instead, per its note.",
  },
  {
    id: "mcp-server-token",
    route: "/app/settings/tokens",
    viewport: DEFAULT_VIEWPORT,
    mask: ["token value cells"],
    persona: "member",
    notes:
      "Personal access tokens list/create page. Shared between integrations/mcp/mcp-server and integrations/third-party-integrations/api-usage — capture once.",
  },
  {
    id: "mcp-tool-selection",
    route: "/app/toolkits/all/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "There is no separate 'Tools tab' — EditToolkit.tsx has only Configuration " +
      "and Indexes tabs (confirmed live: the invented testid timed out); tool " +
      "selection and 'Make tools available by MCP' live on the default " +
      "Configuration tab, captured full-page here. On the autotest_docs_github " +
      "toolkit (or the docs-seed equivalent).",
  },
  {
    id: "mcp-remote-create",
    route: "/app/mcps/create",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Create Remote MCP toolkit form, first screen (server URL field). No OAuth mock exists in the seed, so do not attempt to capture the authorization dialog.",
  },
  {
    id: "mcp-prebuilt-admin",
    route: "/admin/app/configuration",
    viewport: DEFAULT_VIEWPORT,
    // No `[data-testid='admin-mcp-servers-section']` exists (confirmed live:
    // it timed out) — Configuration.tsx's sections are a plain sidebar List of
    // ListItemButtons with no per-section testid; the section is reached by
    // clicking its title text (config_schemas.go's `"title": "MCP Servers"`),
    // same fix as admin-auth-provider/admin-llm-proxy-models/
    // admin-configuration-banner below.
    actions: [{ type: "click", selector: "text=MCP Servers" }],
    mask: ["client secret cells"],
    persona: "admin",
    notes:
      "Admin Configuration page, MCP Servers section (reached by clicking its " +
      "sidebar entry — no dedicated container testid exists). Seed one " +
      "prebuilt entry first via PUT /admin/mcp_prebuilt_servers/administration/{key}.",
  },
  {
    id: "deepwiki-browser",
    route: "/app/deepwiki/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "DeepWiki page browser with the generation panel visible. Needs the real-engine compose overlay and the seeded wiki toolkit in project 90200 (E2E Wiki id 9001); otherwise shoot the empty/no-wiki-yet state.",
  },
  {
    id: "inventory-browser",
    route: "/app/inventory/:toolkitId",
    viewport: DEFAULT_VIEWPORT,
    persona: "member",
    notes:
      "Inventory entity browser with graph filters, on the seeded Inventory toolkit in project 90300 (E2E Inventory id 9101), ingestion completed.",
  },
  {
    id: "admin-overview-nav",
    route: "/admin/app",
    viewport: DEFAULT_VIEWPORT,
    persona: "admin",
    notes:
      "Admin console landing on Users with the sidebar's two nav groups visible.",
  },
  {
    id: "admin-users-table",
    route: "/admin/app/users",
    viewport: DEFAULT_VIEWPORT,
    persona: "admin",
    notes: "Fixture users 90001/90002 visible with row-action menu open.",
  },
  {
    id: "admin-roles-matrix",
    route: "/admin/app/roles",
    viewport: DEFAULT_VIEWPORT,
    persona: "admin",
    notes: "Central scope tab; permission matrix visible.",
  },
  {
    id: "admin-projects-table",
    route: "/admin/app/projects",
    viewport: DEFAULT_VIEWPORT,
    persona: "admin",
    notes: "Fixture projects 90001 (active) and 90002 (suspended).",
  },
  {
    id: "admin-auth-provider",
    route: "/admin/app/configuration",
    viewport: DEFAULT_VIEWPORT,
    // No `[data-testid='configuration-section-*']` exists anywhere
    // (confirmed live on three separate shots) — Configuration.tsx's sidebar
    // is a plain List of ListItemButtons, selected by clicking the section's
    // TITLE text (config_schemas.go's `"title": "Authentication"`).
    actions: [{ type: "click", selector: "text=Authentication" }],
    mask: ["client-secret-field", "certificate-field"],
    persona: "admin",
    notes:
      "Authentication section open (reached by clicking its sidebar entry — no " +
      "dedicated container testid exists); add one oidc provider row (the " +
      "oidc-mock) and one placeholder SAML row; mask secrets.",
  },
  {
    id: "admin-llm-proxy-models",
    route: "/admin/app/configuration",
    viewport: DEFAULT_VIEWPORT,
    actions: [{ type: "click", selector: "text=LLM Proxy" }],
    persona: "admin",
    notes:
      "LLM Proxy section (reached by clicking its sidebar entry — no dedicated " +
      "container testid exists), Models sub-tab; needs a global provider " +
      "(public project shared=true) and gateway_models rows.",
  },
  {
    id: "admin-budgets-table",
    route: "/admin/app/budgets",
    viewport: DEFAULT_VIEWPORT,
    persona: "admin",
    notes: "One budget for project 1, dialog open.",
  },
  {
    id: "admin-governance-table",
    route: "/admin/app/governance",
    viewport: DEFAULT_VIEWPORT,
    persona: "admin",
    notes: "One governance rule seeded.",
  },
  {
    id: "admin-features-sections",
    route: "/admin/app/features",
    viewport: DEFAULT_VIEWPORT,
    persona: "admin",
    notes: "A features section (e.g. Support Assistant) selected.",
  },
  {
    id: "admin-configuration-banner",
    route: "/admin/app/configuration",
    viewport: DEFAULT_VIEWPORT,
    actions: [{ type: "click", selector: "text=Banner" }],
    persona: "admin",
    notes:
      "Banner section (reached by clicking its sidebar entry — no dedicated " +
      "container testid exists) with banner enabled and a message set. Do not " +
      "enable maintenance while shooting other admin pages.",
  },
  {
    id: "admin-branding-preview",
    route: "/admin/app/branding",
    viewport: DEFAULT_VIEWPORT,
    persona: "admin",
    notes: "Default brand pack; preview panel visible.",
  },
  {
    id: "admin-secrets-table",
    route: "/admin/app/secrets",
    viewport: DEFAULT_VIEWPORT,
    mask: ["secret-value-column"],
    persona: "admin",
    notes: "One global secret; mask its value.",
  },
  {
    id: "admin-schedules-table",
    route: "/admin/app/schedules",
    viewport: DEFAULT_VIEWPORT,
    persona: "admin",
    notes: "One schedule row visible on the Schedules tab.",
  },
  {
    id: "admin-tasks-table",
    route: "/admin/app/tasks",
    viewport: DEFAULT_VIEWPORT,
    persona: "admin",
    notes:
      "May be empty if no task is in flight at capture time; still shows the kind filter and columns.",
  },
  {
    id: "admin-app-requests-queue",
    route: "/admin/app/app-requests",
    viewport: DEFAULT_VIEWPORT,
    persona: "admin",
    notes: "One pending app request from the member persona.",
  },
  {
    id: "admin-toolkit-types-table",
    route: "/admin/app/toolkits",
    viewport: DEFAULT_VIEWPORT,
    persona: "admin",
    notes:
      "One restricted toolkit type visible with its reason and worker column.",
  },
  {
    id: "admin-audit-heatmap",
    route: "/admin/app/audit",
    viewport: DEFAULT_VIEWPORT,
    persona: "admin",
    notes:
      "Heatmap plus filtered table; the actions from other admin shots produce rows — capture this shot last.",
  },
];
