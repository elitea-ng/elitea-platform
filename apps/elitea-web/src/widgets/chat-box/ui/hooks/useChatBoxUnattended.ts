/**
 * Unattended mode of the cloud chat (ADR-0045 decisions 1 and 2). The person
 * sets the grant in a dialog before the mode turns on; while it is on, each
 * message they send carries the grant (`unattended` on the send body) with a
 * new grant id, so each send starts a new run chain. Main then answers the
 * grant's questions and starts each continuation by itself.
 *
 * The grant lasts for the conversation on screen only: another conversation
 * starts with the mode off. The sensitive tools that it names must be tools
 * of the conversation's agent; a tool that is not (the agent changed) is
 * dropped from the send.
 */
import { useCallback, useState } from 'react';

import { defaultGrant, newGrantId, toCloudGrant, type SensitiveToolRef, type UnattendedGrantValue } from '@/features/unattended';

import { useStableRef } from './useStableRef';

export interface ChatBoxUnattended {
  /** False where the option does not show (the agent editor's Test panel): the mode stays off. */
  readonly shown: boolean;
  /** The grant while the mode is on; null when it is off. */
  readonly grant: UnattendedGrantValue | null;
  /** The grant in the open dialog; null when the dialog is closed. */
  readonly draft: UnattendedGrantValue | null;
  /** The agent tools the grant can name; undefined when the conversation has no agent. */
  readonly sensitiveTools: readonly SensitiveToolRef[] | undefined;
  readonly open: () => void;
  readonly setDraft: (next: UnattendedGrantValue) => void;
  /** Keep the dialog's grant and turn the mode on. */
  readonly start: () => void;
  readonly cancel: () => void;
  readonly turnOff: () => void;
  /** The `unattended` field of the next send, or undefined while the mode is off. */
  readonly wire: () => Record<string, unknown> | undefined;
}

const sameTool = (a: SensitiveToolRef, b: SensitiveToolRef): boolean => a.toolkit === b.toolkit && a.tool === b.tool;

/** The grant as it goes on the next send: only the named tools that the agent still has. */
export function grantForSend(grant: UnattendedGrantValue | null, tools: readonly SensitiveToolRef[] | undefined, grantId: string): Record<string, unknown> | undefined {
  if (grant === null) return undefined;
  const named = grant.sensitive_tools.filter((tool) => tools?.some((option) => sameTool(option, tool)) === true);
  const result = toCloudGrant({ ...grant, sensitive_tools: named }, grantId);
  return result.ok ? { ...result.grant } : undefined;
}

export function useChatBoxUnattended(conversationKey: string | undefined, sensitiveTools: readonly SensitiveToolRef[] | undefined, shown = true): ChatBoxUnattended {
  const [grant, setGrant] = useState<UnattendedGrantValue | null>(null);
  const [draft, setDraft] = useState<UnattendedGrantValue | null>(null);
  // Another conversation turns the mode off. A new chat that gets its first
  // conversation (no key, then a key) keeps it: that send created it.
  const [shownKey, setShownKey] = useState(conversationKey);
  if (conversationKey !== shownKey) {
    setShownKey(conversationKey);
    if (shownKey !== undefined) {
      setGrant(null);
      setDraft(null);
    }
  }
  const latest = useStableRef({ grant: shown ? grant : null, sensitiveTools });
  const wire = useCallback(() => {
    const current = latest.current;
    return grantForSend(current.grant, current.sensitiveTools, newGrantId());
  }, [latest]);
  return {
    shown,
    grant,
    draft,
    sensitiveTools,
    open: () => setDraft(grant ?? defaultGrant()),
    setDraft,
    start: () => {
      if (draft === null) return;
      setGrant(draft);
      setDraft(null);
    },
    cancel: () => setDraft(null),
    turnOff: () => setGrant(null),
    wire,
  };
}
