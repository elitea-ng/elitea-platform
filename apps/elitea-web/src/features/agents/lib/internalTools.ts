import { useMemo } from 'react';

import { t } from '@/shared/i18n';
import { isInternalToolAvailable, useRuntimeCapabilities } from '@/shared/api/runtimeCapabilities';
import { AgentRobotIcon } from '@/shared/ui/icons/agent-robot-icon';
import { AttachIcon } from '@/shared/ui/icons/attach-icon';
import { CalendarIcon } from '@/shared/ui/icons/calendar-icon';
import { ChatbotIcon } from '@/shared/ui/icons/chatbot-icon';
import { ContextIcon } from '@/shared/ui/icons/context-icon';
import { ImageIcon } from '@/shared/ui/icons/image-icon';
import { McpIcon } from '@/shared/ui/icons/mcp-icon';
import { PieChartIcon } from '@/shared/ui/icons/pie-chart-icon';
import { PythonIcon } from '@/shared/ui/icons/python-icon';
import { SkillIcon } from '@/shared/ui/icons/skill-icon';
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
 * comment) for any tool this endpoint has not been taught to answer for yet.
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
    name: 'ask_user',
    title: 'Ask User',
    icon: 'ChatbotIcon',
    infoTooltip: { text: 'Let the agent ask clarifying questions and wait for your answers before continuing.' },
  },
  {
    // Only the retired Python worker ran this sandbox. It stays in the list so
    // stored agents that toggled it still pass the SQL admission gates; the
    // Rust worker skips it, and GET /elitea_core/runtime_capabilities reports
    // it false, so the switch renders disabled with a reason.
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
    // #1226. Not `swarm`: children start from a fresh context with only their
    // task, never the conversation, and can only read.
    name: 'subagents',
    title: 'Sub-agents',
    icon: 'AgentRobotIcon',
    infoTooltip: {
      text: 'Let the agent hand independent investigations to read-only sub-agents that run in parallel, each with a fresh context, and report back.',
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
  {
    // #940 A8. `useAvailableInternalTools` below reads these two off
    // `/elitea_core/runtime_capabilities` like every other entry, so a
    // deployment that does not serve them renders them disabled-with-a-reason
    // rather than silently doing nothing. Both default to OFF, like every
    // other entry here: a toggle nobody switched on binds no tool
    // (services/elitea-worker-rust/src/agents/internal_tools.rs).
    name: 'skills_builder',
    title: 'Skills Builder',
    icon: 'SkillIcon',
    infoTooltip: { text: 'Create and update Skills directly from chat.' },
  },
  {
    name: 'project_context_builder',
    title: 'Project Context Builder',
    icon: 'ContextIcon',
    infoTooltip: { text: 'Create and update Project Context directly from chat.' },
  },
] as const;

/** `AgentInternalToolSwitch.jsx`'s `iconMap`, re-keyed to this port's icon names (see the module doc comment). */
export const INTERNAL_TOOL_ICONS: Readonly<Record<string, SvgIconComponent>> = {
  AgentRobotIcon,
  AttachIcon,
  CalendarIcon,
  ChatbotIcon,
  ContextIcon,
  ImageIcon,
  McpIcon,
  PieChartIcon,
  PythonIcon,
  SkillIcon,
  SwarmIcon,
  ToolsIcon,
};

/**
 * Why a tool this deployment offers is nonetheless unavailable.
 *
 * Names the worker the endpoint reported. The `''` case is the deployment that
 * did not state a worker at all (see `WithWorkerImplementation`'s own doc
 * comment); it cannot be reached with `available: false`, because an unstated
 * worker makes every tool available, but it is answered rather than asserted
 * away.
 */
function unavailableReasonFor(worker: string | undefined): string {
  if (worker === 'rust') {
    return t('features.agents.internalTools.notAvailableOnRustWorker', 'Not available on the Rust worker');
  }
  return t('features.agents.internalTools.notAvailableOnThisWorker', 'Not available on this worker');
}

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
          ...(available ? {} : { unavailableReason: unavailableReasonFor(capabilities?.worker) }),
        };
      }),
    [toolkitTypeSchemas, includeAgentOnly, isMcpVisible, capabilities],
  );
}
