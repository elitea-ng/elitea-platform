/**
 * Shared local type for this sub-unit (A2h)'s `ui/select/` toolkit/tool
 * pickers (`ToolSelect.jsx`, `LoopToolSelect.jsx`, `ToolkitsSelect.jsx`,
 * `LLMToolsSelect.jsx`) -- one `version_details.tools[]` entry, as read by
 * those four baseline files.
 *
 * A locally-scoped SUPERSET of `../../lib/flow-editor/hooks/
 * useFunctionInputMapping.ts`'s own exported `VersionTool` (reused
 * intra-slice would be R-L3-legal, but that interface is scoped to exactly
 * what `useFunctionInputMapping.hooks.js` itself reads and is missing
 * fields this sub-unit's OWN baseline files read: `agent_type` (baseline:
 * `ToolSelect.jsx:53`, `LoopToolSelect.jsx:60`), `meta.mcp`
 * (`ToolSelect.jsx:35,50`, `LoopToolSelect.jsx:57`), `description`, and
 * `tools`/`settings.selected_tools` (`ToolkitsSelect.jsx:52`,
 * `LoopToolSelect.jsx:86-92`)). Declared fresh here rather than widening
 * the other file's exported type out from under its own owning sub-unit.
 */
export interface PipelineToolEntry {
  readonly id?: string;
  readonly type?: string;
  readonly name?: string;
  readonly toolkit_name?: string;
  readonly description?: string;
  /** `'pipeline'` when `type === 'application'` names a pipeline rather than an agent (baseline: `tool.agent_type === 'pipeline'`). */
  readonly agent_type?: string;
  readonly meta?: { readonly mcp?: boolean };
  /** Connection state updated by `usePipelineMCPToolsStatusMonitor` for MCP tools. */
  readonly online?: boolean;
  /** Static tool-name list on a non-dynamic toolkit entry (`ToolkitsSelect.jsx:52`'s `toolkitObj?.tools`). */
  readonly tools?: readonly (string | { readonly name?: string })[];
  readonly settings?: Readonly<Record<string, unknown>> & {
    readonly selected_tools?: readonly string[];
    readonly application_id?: number;
    readonly application_version_id?: number;
  };
}
