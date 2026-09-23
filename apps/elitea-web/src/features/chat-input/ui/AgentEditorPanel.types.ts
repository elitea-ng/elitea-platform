import type { Participant } from '@/entities/participant';
import type { VersionSummary } from '@/entities/version';

import type { AgentVariable } from './VariablesEditor.types';

/**
 * Type module for `AgentEditorPanel.tsx`, split out purely to keep that
 * file (and its per-function cyclomatic complexity) under the §3.5
 * budgets — same rationale as `UserInput.types.ts`.
 */

/** The "fetched agent/pipeline version details" blob — baseline: `participantDetails` (old `activeParticipantDetails`). No existing entities/ type covers this shape (it is neither a `Participant` nor a full `Version`). */
export interface AgentEditorParticipantDetails {
  readonly id?: string;
  readonly name?: string;
  readonly versions?: readonly VersionSummary[];
}

/**
 * Not exported beyond this module (knip: no import of any of these three
 * from outside `AgentEditorPanel.{ts,tsx}` anywhere in the repo) — each is
 * only ever named as a nested field type of `AgentEditorPanelProps` below.
 */
interface AgentEditorVersionProps {
  readonly selectedVersionId?: string | undefined;
  readonly onSelect: (version: VersionSummary) => void;
  readonly onShowVersionChangeAlert?: ((proceed: () => void) => void) | undefined;
  /** Baseline gap fix, disclosed in `AgentEditorPanel.tsx`'s own module doc: wired through to `VersionSelector`'s `onRefresh`, which the baseline never did. */
  readonly onRefresh?: (() => Promise<void> | void) | undefined;
}

interface AgentEditorVariablesProps {
  readonly variables: readonly AgentVariable[];
  readonly onChange: (variables: readonly AgentVariable[]) => void;
}

interface AgentEditorNavProps {
  readonly onShowAgentEditor?: ((participant: Participant) => void) | undefined;
  readonly onShowPipelineEditor?: ((participant: Participant) => void) | undefined;
  readonly onCloseAgentEditor?: (() => void) | undefined;
  readonly onClosePipelineEditor?: (() => void) | undefined;
}

/** @public §3.5 budget: 11 top-level props (grouped). */
export interface AgentEditorPanelProps {
  readonly activeParticipant: Participant | undefined;
  readonly participantDetails: AgentEditorParticipantDetails | undefined;
  readonly disabled?: boolean | undefined;
  readonly disableSwitchToModel?: boolean | undefined;
  readonly isEditorDirty?: boolean | undefined;
  readonly onClickParticipant?: (() => void) | undefined;
  readonly onSwitchToModel?: (() => void) | undefined;
  readonly version: AgentEditorVersionProps;
  readonly variablesEditor: AgentEditorVariablesProps;
  readonly editorNav: AgentEditorNavProps;
  /**
   * A14 (ELITEA-0386): opens the composition root's LLM-settings dialog for
   * `activeParticipant` — see `widgets/chat-box/ui/ChatBoxLlmSettingsDialog
   * .tsx`'s own module doc for why this stays a plain callback (this feature
   * may not import `widgets/llm-model-selector` — R-L1). Omitted (`undefined`)
   * withholds the button entirely, same convention as `onSwitchToModel`.
   */
  readonly onEditLlmSettings?: (() => void) | undefined;
}
