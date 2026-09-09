import { useMemo } from 'react';

import { t } from '@/shared/i18n';
import { isInternalToolAvailable, useRuntimeCapabilities } from '@/shared/api/runtimeCapabilities';
import { AttachIcon } from '@/shared/ui/icons/attach-icon';
import { CalendarIcon } from '@/shared/ui/icons/calendar-icon';
import { ImageIcon } from '@/shared/ui/icons/image-icon';
import { McpIcon } from '@/shared/ui/icons/mcp-icon';
import { PieChartIcon } from '@/shared/ui/icons/pie-chart-icon';
import { PythonIcon } from '@/shared/ui/icons/python-icon';
import { SwarmIcon } from '@/shared/ui/icons/swarm-icon';
import { ToolsIcon } from '@/shared/ui/icons/tools-icon';
import type { SvgIconComponent } from '@/shared/ui/icons/svg-icon.types';

import { useIsMcpVisible } from '../api/useIsMcpVisible';
import { useToolkitTypeSchemas } from '../api/useToolkitTypeSchemas';

import { useSelectedProjectId } from '../api/useSelectedProjectId';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/shared/lib/constants/
 * internalTools.constants.js`'s `INTERNAL_TOOLS_LIST` — field subset actually
 * read by this sub-unit's owned files (`ApplicationTools`,
 * `AgentInternalToolSwitch`): `name`, `title`, `icon`, `infoTooltip`,
 * `agentOnly`, `requiredToolkitType`. `toolkitNames` (used elsewhere in the
 * baseline for a toolkit<->internal-tool cross-reference outside this
 * sub-unit's scope) is dropped — not read by anything owned here.
 *
 * Icon names are mapped to this app's real `shared/ui/icons/*` ports
 * (unit S2) in `INTERNAL_TOOL_ICONS` below, replacing the baseline's
 * string-keyed `iconMap` indirection in `AgentInternalToolSwitch.jsx`.
 */
export interface InternalToolInfoTooltip {
  readonly text: string;
  readonly linkText?: string | undefined;
  readonly linkUrl?: string | undefined;
  readonly suffix?: string | undefined;
}

export interface InternalToolDescriptor {
  readonly name: string;
  readonly title: string;
  readonly icon: string;
  readonly infoTooltip: InternalToolInfoTooltip;
  readonly agentOnly?: boolean;
  readonly requiredToolkitType?: string;
}

/**
 * {@link InternalToolDescriptor} plus the CONFIGURED worker's real verdict on
 * it (#865, #866), computed dynamically from `GET
 * /elitea_core/runtime_capabilities` — it cannot be part of the static
 * `INTERNAL_TOOLS_LIST` below, which describes every deployment the same way.
 *
 * `available` defaults to `true` (see `isInternalToolAvailable`'s own doc
 * comment) for any tool this endpoint has not been taught to answer for yet
 * — currently `attachments` and `pyodide`, #866's own disclosed scope
 * boundary — so those two keep behaving exactly as before this change.
 */
export interface AvailableInternalTool extends InternalToolDescriptor {
  readonly available: boolean;
  /** Set only when `available` is false — the switch's disabled tooltip reads this. */
  readonly unavailableReason?: string;
}

/** Toolkit type key used to check if image generation is available via provider plugin. */
const IMAGE_GENERATION_TOOLKIT_TYPE = 'ImageGenServiceProvider_ImageGen';

export const INTERNAL_TOOLS_LIST: readonly InternalToolDescriptor[] = [
  {
    name: 'attachments',
    title: 'Attachments',
    icon: 'AttachIcon',
    infoTooltip: {
      text: 'Enable file attachment capabilities for document upload, indexing, and search operations in conversations.',
    },
    agentOnly: true,
  },
  {
    name: 'image_generation',
    title: 'Image creation',
    icon: 'ImageIcon',
    infoTooltip: { text: 'Enable AI-powered image generation capabilities.' },
    requiredToolkitType: IMAGE_GENERATION_TOOLKIT_TYPE,
  },
  {
    name: 'data_analysis',
    title: 'Data Analysis',
    icon: 'PieChartIcon',
    infoTooltip: {
      text: 'Enable data analysis capabilities using.',
      linkText: 'Pandas',
      linkUrl: 'https://pandas.pydata.org/docs/',
      suffix: '. Works with files from conversation attachments.',
    },
  },
  {
    name: 'internal_mcp',
    title: 'Elitea MCP Tools',
    icon: 'McpIcon',
    infoTooltip: {
      text: 'Enable Elitea platform MCP tools for managing applications (agents and pipelines), chat, and toolkits directly from conversations.',
    },
  },
  {
    name: 'planner',
    title: 'Planner',
    icon: 'CalendarIcon',
    infoTooltip: { text: 'Enable managing and tracking todo items for task planning.' },
  },
  {
    // #872: as of the Python worker's image, this toggle actually executes
    // (a pinned `deno` binary plus the SDK's own sandbox entrypoint, both
    // baked in at build time — services/elitea-worker-python/Containerfile).
    // The Rust worker still skips it, the same as every other entry in this
    // list without a `requiredToolkitType` gate. GET /elitea_core/
    // runtime_capabilities (#865, #866) now exists and would answer this the
    // same way it answers for the six tools `useAvailableInternalTools`
    // checks against it, but its `internal_tools` map deliberately does not
    // name `pyodide` (nor `attachments`) — see
    // internal/api/v2/toolkits/capabilities_handler.go's own doc comment for
    // why those two stayed out of #866's scope. So this toggle still cannot
    // be conditionally disabled the way the six others now are. See
    // /menus/internal-tools for the operator-facing note instead.
    name: 'pyodide',
    title: 'Python sandbox',
    icon: 'PythonIcon',
    infoTooltip: {
      text: 'Enable Python code execution in a secure sandbox using',
      linkText: 'Pyodide',
      linkUrl: 'https://pyodide.org/en/stable/usage/packages-in-pyodide.html',
      suffix: '.',
    },
  },
  {
    name: 'swarm',
    title: 'Swarm Mode',
    icon: 'SwarmIcon',
    infoTooltip: {
      text: 'Enable swarm-style multi-agent collaboration. When enabled, all child agents share the full conversation history and can hand off control to each other.',
    },
  },
  {
    name: 'lazy_tools_mode',
    title: 'Smart Tools Selection',
    icon: 'ToolsIcon',
    infoTooltip: {
      text: 'Reduces token usage by using meta-tools instead of binding all tools directly. Recommended when using many toolkits.',
    },
  },
] as const;

/** `AgentInternalToolSwitch.jsx`'s `iconMap`, re-keyed to this port's icon names (see the module doc comment). */
export const INTERNAL_TOOL_ICONS: Readonly<Record<string, SvgIconComponent>> = {
  AttachIcon,
  CalendarIcon,
  ImageIcon,
  McpIcon,
  PieChartIcon,
  PythonIcon,
  SwarmIcon,
  ToolsIcon,
};

export interface UseAvailableInternalToolsOptions {
  readonly includeAgentOnly?: boolean;
}

/**
 * Ported from `apps/elitea-ui/src/[fsd]/shared/lib/hooks/
 * useAvailableInternalTools.hooks.js`. Filters `INTERNAL_TOOLS_LIST` down to
 * tools OFFERED at all: agent-only tools require `includeAgentOnly`,
 * `internal_mcp` requires `useIsMcpVisible()`, and any tool naming a
 * `requiredToolkitType` requires that type to be present in the project's
 * toolkit-type schema map. A tool this project has no provider for is
 * dropped entirely — there is nothing a user can do about it from here.
 *
 * A DIFFERENT question, added by #865/#866, is layered on TOP rather than
 * filtered by: whether the CONFIGURED WORKER runs an offered tool for real.
 * That answer (`useRuntimeCapabilities`) does not remove the tool from this
 * list — it stays visible with `available: false` and a reason, so
 * `AgentInternalToolSwitch` can render it disabled with an explanation
 * instead of the pre-#865/#866 behaviour of vanishing with no trace the
 * toggle ever existed.
 */
export function useAvailableInternalTools(options: UseAvailableInternalToolsOptions = {}): readonly AvailableInternalTool[] {
  const { includeAgentOnly = false } = options;
  const projectId = useSelectedProjectId();
  const { toolkitTypeSchemas } = useToolkitTypeSchemas(projectId);
  const isMcpVisible = useIsMcpVisible();
  const capabilities = useRuntimeCapabilities();

  return useMemo(
    () =>
      INTERNAL_TOOLS_LIST.filter((tool) => {
        if (tool.agentOnly && !includeAgentOnly) return false;
        if (tool.name === 'internal_mcp' && !isMcpVisible) return false;
        if (!tool.requiredToolkitType) return true;
        return Boolean(toolkitTypeSchemas?.[tool.requiredToolkitType]);
      }).map((tool) => {
        const available = isInternalToolAvailable(capabilities, tool.name);
        return {
          ...tool,
          available,
          ...(available
            ? {}
            : {
                unavailableReason: t(
                  'features.agents.internalTools.notAvailableOnRustWorker',
                  'Not available on the Rust worker',
                ),
              }),
        };
      }),
    [toolkitTypeSchemas, includeAgentOnly, isMcpVisible, capabilities],
  );
}
