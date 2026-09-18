import type { ReactNode } from 'react';
import { useCallback } from 'react';

import { CredentialWarningBanner } from '@/entities/credential';

import { useBlockedToolkitTypes } from '../api/useBlockedToolkitTypes';
import { useDisassociateToolkit } from '../lib/hooks/useDisassociateToolkit.hooks';
import type { ToolRemovalUpdate } from '../lib/hooks/useDisassociateToolkit.hooks';
import { useSaveSelectedTools } from '../lib/hooks/useSaveSelectedTools.hooks';
import { useToolkitCredentialIssue } from '../lib/hooks/useToolkitCredentialIssue';
import type { AgentToolAssociation } from '../lib/types';

import { ToolCard } from './ToolCard';

/**
 * ONE attached tool, as a component rather than a `renderToolCard` inline
 * closure — `useDisassociateToolkit` takes the row's `index` and is one hook
 * call per row, and React hooks cannot be called from a `.map()` in the
 * parent. This is the whole reason `ApplicationTools` takes an injected
 * `renderToolCard` render-prop instead of rendering `ToolCard` itself (see
 * that file's own module doc comment): the caller owns the per-row state,
 * so the caller owns the per-row component too.
 *
 * `AgentToolsPanel` (sibling) is the only caller; this file exists
 * separately purely so both stay inside the §3.5 file-length/complexity
 * budgets.
 *
 * **`ToolCard` prop groups this row deliberately does NOT fill, each a real
 * gap rather than an oversight:**
 *  - `versionSelector` (sub-agent/pipeline version dropdown) — omitted, and
 *    `ToolCardBody` gates the whole selector on it being present, so nothing
 *    renders rather than an empty dropdown. Switching an attached sub-agent's
 *    version is `useApplicationChatSwitchVersion`/`useSaveChangedTools`
 *    territory and needs a version LIST per attached sub-agent, which no
 *    endpoint on this page's fetch returns.
 *  - `validation.onRevalidate` — still omitted: this app's generated
 *    validate-version endpoint returns a plain `{valid}` with none of the
 *    per-toolkit detail a re-check could report (disclosed in
 *    `ToolCard.types.ts`). **`validation.banner` IS filled now (#937)**, from
 *    the one signal that does not need that endpoint: whether the credential
 *    this toolkit names still exists — see `useToolkitCredentialIssue`. Until
 *    it was, the slot had no caller anywhere, so "Credential setup required:"
 *    could not appear on an Agent's, a Pipeline's or a Chat participant's tool
 *    card at all.
 *  - `delegatedAuth` — `features/mcps`/`features/sharepoint`/`features/openapi`
 *    slots; `no-sideways-features` forbids reaching them from here, and a
 *    page-level composition would have to inject them.
 *  - **`variables` — WITHHELD ON PURPOSE (#248), and this is why `ToolCard`
 *    renders no variables toggle and no variables panel for these rows.**
 *    There is no per-tool variables column and no tool-variables table
 *    anywhere in the schema: `p_{id}.entity_tool_mapping` is
 *    `(id, entity_version_id, entity_id, entity_type, tool_id,
 *    selected_tools, created_at, updated_at)` and nothing else stores them,
 *    `applications`' `UpdateVersion` has no `tools` branch, and
 *    `fetchVersionDetails` emits no `variables` key on a tool row either — so
 *    the values could be neither written nor read back. An editable field
 *    that accepts input and discards it is precisely the defect class this
 *    change removes, so the control is not offered at all rather than offered
 *    and disabled: the legacy baseline
 *    (`EliteaUI/src/pages/Applications/Components/Tools/ToolCard.jsx:453-473`)
 *    gates that toggle on `hasVariables` alone and simply omits it when there
 *    is nothing behind it, and the conversation-starters port established
 *    hide-not-disable as this port's convention for the same situation.
 *    Restoring it means adding storage first — and then a caller passing
 *    `ToolCard`'s still-supported `variables` prop group.
 *
 * **`toolSelection.onSelectedToolsChange` DOES persist (#248).** It updates
 * the panel's in-memory `tools[]` mirror AND issues the relation PATCH with
 * the full new list, which the Go handler now writes to
 * `entity_tool_mapping.selected_tools` (upserting on `_entity_tool_unique`)
 * and `fetchVersionDetails` reads straight back into
 * `version_details.tools[].selected_tools`. Like attach and detach, it does
 * not go through the page's Save button — see `useSaveSelectedTools.hooks.ts`.
 */
/** Structural only — a caller builds this inline against `AgentToolRowProps`; un-exported so knip does not flag an unused named export (the `ToolCardIcon` precedent in `ToolCard.types.ts`). */
interface AgentToolRowEntity {
  readonly applicationId: number | undefined;
  readonly versionId: number | undefined;
  readonly projectId: string | undefined;
  /** `version_details.meta.attachment_toolkit_id` — drives the attachment-toolkit paperclip and remove-dialog copy. */
  readonly attachmentToolkitId?: number | string | undefined;
}

/** Structural only, see `AgentToolRowEntity` above. */
interface AgentToolRowToolsState {
  readonly tools: readonly AgentToolAssociation[];
  /** The "Discard" baseline — `useDisassociateToolkit` rebases it as well as the live array on a successful removal. */
  readonly initialTools: readonly AgentToolAssociation[];
  /** Whether the host form already had unsaved edits BEFORE this removal (gates the hook's `setRefetch`). */
  readonly dirty: boolean;
  readonly onToolsChange: (tools: readonly AgentToolAssociation[]) => void;
  readonly onToolRemoved: (update: ToolRemovalUpdate) => void;
}

export interface AgentToolRowProps {
  readonly tool: AgentToolAssociation;
  readonly index: number;
  readonly isDuplicate: boolean;
  readonly disabled: boolean;
  readonly viewMode: string;
  readonly entity: AgentToolRowEntity;
  readonly toolsState: AgentToolRowToolsState;
}

export function AgentToolRow({ tool, index, isDuplicate, disabled, viewMode, entity, toolsState }: AgentToolRowProps): ReactNode {
  const { onDisassociateTool, isLoading } = useDisassociateToolkit({
    applicationId: entity.applicationId,
    versionId: entity.versionId,
    index,
    tools: toolsState.tools,
    initialTools: toolsState.initialTools,
    dirty: toolsState.dirty,
    onToolRemoved: toolsState.onToolRemoved,
    // `onDeleteAttachmentTool` (the hook's "the removed tool WAS the
    // attachment toolkit, clear `meta.attachment_toolkit_id`" signal) is
    // deliberately not passed: nothing in this composition sets that meta key
    // — the attachment toolkit is `AttachmentSwitch`'s territory and it is not
    // mounted on this page. A pass-through callback with no state behind it
    // would be dead wiring, which this codebase has been bitten by repeatedly.
  });

  const { onSelectedToolsChange } = useSaveSelectedTools({
    tool,
    index,
    applicationId: entity.applicationId,
    versionId: entity.versionId,
    tools: toolsState.tools,
    onToolsChange: toolsState.onToolsChange,
  });

  // The guardrails blocklist, finally sourced. `ToolCard.types.ts` declared
  // this field and no production caller ever filled it, so `isBlockedToolkit`
  // was structurally always false and the "blocked by your organization"
  // banner could not appear on any screen — see the hook's own header.
  const blockedToolkitTypes = useBlockedToolkitTypes();

  // #937: the credential this toolkit names, checked against the project that
  // would hold it. `null` whenever there is no reference, no verdict, or the
  // credential resolves — the banner is a claim, not a default.
  const credentialIssue = useToolkitCredentialIssue(tool.settings, entity.projectId);

  const handleDisassociate = useCallback(
    ({ isAttachmentToolkit }: { readonly isAttachmentToolkit: boolean }) => {
      void onDisassociateTool({ tool, isAttachmentToolkit });
    },
    [onDisassociateTool, tool],
  );

  return (
    <ToolCard
      tool={tool}
      disabled={disabled}
      isDuplicate={isDuplicate}
      context={{
        selectedProjectId: entity.projectId,
        viewMode,
        versionId: entity.versionId,
        attachmentToolkitId: entity.attachmentToolkitId,
        blockedToolkitTypes,
      }}
      icon={{ url: tool.icon_meta?.url }}
      disassociate={{ onDisassociateTool: handleDisassociate, isDisassociating: isLoading }}
      toolSelection={{ onSelectedToolsChange }}
      {...(credentialIssue === null
        ? {}
        : {
            validation: {
              hasIssue: true,
              banner: (
                <CredentialWarningBanner
                  credentialId={credentialIssue.reference.eliteaTitle}
                  credentialType={credentialIssue.reference.credentialType}
                  section="credentials"
                  createHref={credentialIssue.createHref}
                />
              ),
            },
          })}
    />
  );
}
