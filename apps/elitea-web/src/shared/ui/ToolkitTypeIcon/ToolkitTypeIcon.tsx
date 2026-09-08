/**
 * Ported from `apps/elitea-ui/src/common/toolkitUtils.jsx`'s `getToolIcon`
 * (the per-toolkit-type brand-glyph switch `useToolkitSearch.js` calls for
 * every tile in the type catalogue).
 *
 * This closes — for the type-catalogue screens — the gap
 * `features/toolkits/lib/helpers/toolkits.helpers.ts` (module doc, point 4)
 * and `features/toolkits/lib/hooks/useToolMenuItems.ts` both disclose: "this
 * app has no per-brand toolkit-icon asset library". It does now; S2 ported
 * ~198 glyphs and nothing ever wired a TYPE to one, so every tile in "New
 * Toolkit"/"New MCP" rendered with no icon at all while the reference shows
 * one on each. `github-icon`/`gitlab-icon`/`confluence-icon` were the three
 * the baseline keeps as inline JSX rather than as assets — they are added
 * alongside this file.
 *
 * DISCLOSED SUBSTITUTIONS (a glyph this app genuinely does not have, mapped
 * to the nearest ported one rather than invented):
 *  - `confluence` -> `ConfluenceIcon` (ported here), `ado_boards`/`ado_wiki`/
 *    `ado` -> `AdoGeneralIcon` (baseline's own `AdoIcon` for `ado_boards`/
 *    `ado_wiki`).
 *  - `vertex_ai` -> `GoogleIcon` (baseline `VertexAIIcon`, not ported).
 *  - `azure_open_ai` -> `AzureIcon` (baseline reuses its `AdoIcon`; the
 *    dedicated Azure glyph IS ported here and is the better match).
 *  - `yagmail` (baseline `EmailIcon`), `open_ai`, `hugging_face` have no
 *    ported equivalent and fall through to the generic `ToolIcon` — the
 *    baseline's own `BuildIcon` default.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { combineSx } from '../lib/combineSx';

import { AdoGeneralIcon } from '../icons/ado-general-icon';
import { AdoPlansIcon } from '../icons/ado-plans-icon';
import { AdoReposIcon } from '../icons/ado-repos-icon';
import { AmazonBedrockIcon } from '../icons/amazon-bedrock-icon';
import { ApplicationsIcon } from '../icons/applications-icon';
import { ArtifactsIcon } from '../icons/artifacts-icon';
import { AzureIcon } from '../icons/azure-icon';
import { BitbucketIcon } from '../icons/bitbucket-icon';
import { BrowserUseIcon } from '../icons/browser-use-icon';
import { ChromaIcon } from '../icons/chroma-icon';
import { CodeIcon } from '../icons/code-icon';
import { ConfluenceIcon } from '../icons/confluence-icon';
import { DialIcon } from '../icons/dial-icon';
import { EmbeddingsIcon } from '../icons/embeddings-icon';
import { FigmaIcon } from '../icons/figma-icon';
import { FlowIcon } from '../icons/flow-icon';
import { GithubIcon } from '../icons/github-icon';
import { GitlabIcon } from '../icons/gitlab-icon';
import { GitlabSpaceIcon } from '../icons/gitlab-space-icon';
import { GoogleIcon } from '../icons/google-icon';
import { GplacesIcon } from '../icons/gplaces-icon';
import { JiraIcon } from '../icons/jira-icon';
import { JsonIcon } from '../icons/json-icon';
import { LlmIcon } from '../icons/llm-icon';
import { McpIcon } from '../icons/mcp-icon';
import { MemoryIcon } from '../icons/memory-icon';
import { OllamaIcon } from '../icons/ollama-icon';
import { PostgreSqlIcon } from '../icons/postgre-sql-icon';
import { PostmanIcon } from '../icons/postman-icon';
import { PptxIcon } from '../icons/pptx-icon';
import { QtestIcon } from '../icons/qtest-icon';
import { RallyIcon } from '../icons/rally-icon';
import { ReportportalIcon } from '../icons/reportportal-icon';
import { SalesforceIcon } from '../icons/salesforce-icon';
import { ServiceNowIcon } from '../icons/service-now-icon';
import { SharepointIcon } from '../icons/sharepoint-icon';
import { SlackIcon } from '../icons/slack-icon';
import { SonarIcon } from '../icons/sonar-icon';
import { SqlIcon } from '../icons/sql-icon';
import { TestioIcon } from '../icons/testio-icon';
import { TestrailIcon } from '../icons/testrail-icon';
import { ToolIcon } from '../icons/tool-icon';
import { XrayIcon } from '../icons/xray-icon';
import { ZephyrIcon } from '../icons/zephyr-icon';
import type { SvgIconComponent } from '../icons/svg-icon.types';

const TOOLKIT_TYPE_ICONS: Readonly<Record<string, SvgIconComponent>> = {
  ado: AdoGeneralIcon,
  ado_boards: AdoGeneralIcon,
  ado_plans: AdoPlansIcon,
  ado_repos: AdoReposIcon,
  ado_wiki: AdoGeneralIcon,
  agent: ApplicationsIcon,
  ai_dial: DialIcon,
  amazon_bedrock: AmazonBedrockIcon,
  application: ApplicationsIcon,
  artifact: ArtifactsIcon,
  azure_open_ai: AzureIcon,
  bitbucket: BitbucketIcon,
  browser: BrowserUseIcon,
  chroma: ChromaIcon,
  confluence: ConfluenceIcon,
  custom: JsonIcon,
  embedding_model: EmbeddingsIcon,
  figma: FigmaIcon,
  github: GithubIcon,
  gitlab: GitlabIcon,
  gitlab_org: GitlabSpaceIcon,
  google_places: GplacesIcon,
  jira: JiraIcon,
  llm_model: LlmIcon,
  mcp: McpIcon,
  memory: MemoryIcon,
  ollama: OllamaIcon,
  open_api: CodeIcon,
  openapi: CodeIcon,
  pgvector: PostgreSqlIcon,
  pipeline: FlowIcon,
  postman: PostmanIcon,
  pptx: PptxIcon,
  qtest: QtestIcon,
  rally: RallyIcon,
  report_portal: ReportportalIcon,
  salesforce: SalesforceIcon,
  service_now: ServiceNowIcon,
  sharepoint: SharepointIcon,
  slack: SlackIcon,
  sonar: SonarIcon,
  sql: SqlIcon,
  testio: TestioIcon,
  testrail: TestrailIcon,
  vertex_ai: GoogleIcon,
  xray_cloud: XrayIcon,
  // Baseline `getToolIcon` lists all five Zephyr editions against one glyph.
  zephyr: ZephyrIcon,
  zephyr_enterprise: ZephyrIcon,
  zephyr_essential: ZephyrIcon,
  zephyr_scale: ZephyrIcon,
  zephyr_squad: ZephyrIcon,
};

/** @public The glyph for a toolkit/credential TYPE key, or the generic tool glyph. */
export function toolkitTypeIconComponent(type: string | undefined): SvgIconComponent {
  if (type === undefined) return ToolIcon;
  return TOOLKIT_TYPE_ICONS[type.toLowerCase()] ?? ToolIcon;
}

export interface ToolkitTypeIconProps {
  readonly type: string | undefined;
  readonly sx?: SxProps<Theme>;
}

/**
 * Colour only — no size. The baseline passes a 16px glyph into a 1.25rem
 * container whose own `& svg { width: 100% }` rule scales it up
 * (`CategoryItemCard`'s `itemIconContainer`), so pinning a size here would
 * fight the container instead of filling it.
 */
const iconSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.icon.fill.default,
});

/** @public Renders {@link toolkitTypeIconComponent} at the catalogue tile's 1rem size. */
export function ToolkitTypeIcon({ type, sx }: ToolkitTypeIconProps): ReactNode {
  return (
    <Box
      component={toolkitTypeIconComponent(type)}
      aria-hidden="true"
      sx={combineSx(iconSx, sx)}
    />
  );
}
