import type { ReactNode } from 'react';

/**
 * Shared local types for the A1h sub-unit ("Tool-card composition UI") —
 * `AgentPipelineVersionSelector.jsx`, `AgentVariables.jsx`,
 * `EnhancedCardToolActions.jsx`, `BaseCardBody.jsx`, `ToolCard.jsx`, all from
 * `apps/elitea-ui/src/pages/Applications/Components/Tools/`.
 *
 * `AgentToolAssociation` is a local, minimal shape for one entry of an
 * application version's `tools[]` array — a "toolkit/agent/pipeline
 * attached to this agent" association row (NOT the same thing as
 * `entities/toolkit`'s `Toolkit`, which is a toolkit's OWN catalogue
 * record). No promoted `entities/` type covers this shape:
 * `entities/application-form`'s `ApplicationVersionDraft.tools` is
 * deliberately `readonly unknown[]` (its own doc comment: no generated
 * endpoint carries a `tools` field on write, so it never had reason to type
 * the entries), and `entities/toolkit`'s `Toolkit` models a toolkit's
 * catalogue record, not an association row embedded in an application
 * version (a row that also covers `type: 'application'` sub-agent/pipeline
 * entries, which are never toolkits at all). Fields below are exactly the
 * ones read across this sub-unit's 5 owned files — verified by reading each
 * file in full, not assumed from the type's name.
 */
export interface AgentToolVariable {
  readonly name: string;
  readonly value: string;
}

export interface AgentToolAvailableToolOption {
  readonly label?: string | undefined;
  readonly name?: string | undefined;
  readonly value?: string | undefined;
}

/** Structural only (not separately imported anywhere — knip flags an unused named export otherwise); nested inside `AgentToolAssociation.settings` below. */
interface AgentToolSettings {
  readonly url?: string | undefined;
  readonly sharepoint_configuration?: unknown;
  readonly openapi_configuration?: unknown;
  readonly oauth_discovery_endpoint?: string | undefined;
  readonly configuration_title?: string | undefined;
  readonly selected_tools?: readonly string[] | undefined;
  readonly available_tools?: readonly (string | AgentToolAvailableToolOption)[] | undefined;
  readonly application_id?: string | number | undefined;
  readonly application_version_id?: string | number | undefined;
}

/** Structural only, see `AgentToolSettings` above; nested inside `AgentToolAssociation.icon_meta`. */
interface AgentToolIconMeta {
  readonly component?: ReactNode | undefined;
  readonly url?: string | undefined;
}

/**
 * `tool.meta` — old app: `{ mcp?: boolean, attachment_toolkit_id?: ... }`
 * lives one level up on `version_details.meta`, not on the tool itself; the
 * tool's own `meta.mcp` is the only `tool.meta.*` field this cluster reads
 * (`ToolCard.jsx:68`). Structural only, see `AgentToolSettings` above.
 */
interface AgentToolMeta {
  readonly mcp?: boolean | undefined;
}

export interface AgentToolAssociation {
  /** `entity_tool_mapping.id` (or `application_tools.id`) — the ASSOCIATION row's own id, not the toolkit's. */
  readonly id?: string | number | undefined;
  /**
   * `elitea_tools.id` — the attached toolkit INSTANCE's id, present on every
   * `entity_tool_mapping`-sourced row (`applications/handler.go`'s
   * `fetchVersionDetails` selects `etm.tool_id` alongside `etm.id`). This is
   * the id the attach/detach relation endpoint is keyed by; see
   * `lib/toolRelation.ts`'s `resolveToolkitId`.
   */
  readonly tool_id?: string | number | undefined;
  readonly type?: string | undefined;
  readonly name?: string | undefined;
  readonly elitea_title?: string | undefined;
  readonly toolkit_name?: string | undefined;
  readonly description?: string | undefined;
  readonly online?: boolean | undefined;
  /** `'pipeline' | undefined` — only meaningful when `type === 'application'`. */
  readonly agent_type?: string | undefined;
  readonly meta?: AgentToolMeta | undefined;
  readonly settings?: AgentToolSettings | undefined;
  readonly variables?: readonly AgentToolVariable[] | undefined;
  readonly icon_meta?: AgentToolIconMeta | undefined;
}

/** One entry of `AgentPipelineVersionSelector`'s version dropdown — ported field subset of the baseline's `applicationData.versions[]` entry (`AgentPipelineVersionSelector.jsx:62-71`). */
export interface AgentPipelineVersionOption {
  readonly id: number;
  readonly name: string;
  readonly created_at?: string | undefined;
  readonly status?: string | undefined;
  /**
   * Whether this is the application's default version, as the SERVER reports
   * it: `GET /application/...`'s `versions[].is_default`, derived by
   * `services/elitea-main/internal/api/v2/applications/handler.go`'s
   * `getVersions` from `applications.meta.default_version_id`.
   *
   * This is what lets the version bar show the default on FIRST render.
   * Before the read existed, `AgentVersionControls` could only know the
   * default it had itself just set, so a reload lost it.
   *
   * Optional because not every list that feeds this option shape carries it —
   * `undefined` means "this list cannot say", which the selector treats the
   * same as `false` rather than as "no default exists".
   */
  readonly is_default?: boolean | undefined;
  /**
   * Issue 940/A11 (ELITEA-3278) — the version's creator, read the same way
   * `is_default` above is: off the wire, ahead of `ApplicationVersionSummary`
   * (the generated OpenAPI schema) actually modeling it, via a narrow cast
   * (`editApplicationMappers.ts`'s `readAuthor`/`editPipelineMappers.ts`'s
   * twin). `services/elitea-main/internal/api/v2/applications/handler.go`'s
   * `getVersions` now answers `versions[].author = {id, email, name}`,
   * absent when the version genuinely has no recorded author. This is what
   * the version-selector dropdown's search box filters by, alongside name.
   */
  readonly author?: { readonly id?: string; readonly email?: string; readonly name?: string } | undefined;
}
