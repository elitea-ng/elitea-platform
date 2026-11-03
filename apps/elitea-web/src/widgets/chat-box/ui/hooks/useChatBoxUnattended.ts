/**
 * "Run to completion" in the cloud chat (ADR-0045 decisions 1 and 2). It is
 * on by default: each message the person sends carries a grant (`unattended`
 * on the send body) with a new grant id, so each send starts a new run chain
 * that continues at the step limit and at the output limit, up to the chain
 * limits. The sensitive tools stay off until the person selects them.
 *
 * The choice is kept per conversation for this page load. A new conversation
 * starts on, with the default grant. A new chat that gets its first
 * conversation (no key, then a key) keeps its choice: that send created it.
 * The sensitive tools that the grant names must be tools of the
 * conversation's agent; a tool that is not (the agent changed) is dropped
 * from the send. When an administrator turned grants off, the control hides
 * and no grant goes out (`useGrantsTurnedOff`).
 */
import { useCallback, useState } from 'react';

import {
  defaultChoice,
  grantToSend,
  grantsTurnedOff,
  newGrantId,
  toCloudGrant,
  useGrantsTurnedOff,
  type RunChoice,
  type SensitiveToolRef,
} from '@/features/unattended';

import { useStableRef } from './useStableRef';

export interface ChatBoxUnattended {
  /** False where the control does not show (the agent editor's Test panel, or grants are off): no grant goes out. */
  readonly shown: boolean;
  readonly choice: RunChoice;
  readonly setChoice: (next: RunChoice) => void;
  /** The agent tools the grant can name; undefined when the conversation has no agent. */
  readonly sensitiveTools: readonly SensitiveToolRef[] | undefined;
  /** The `unattended` field of the next send, or undefined while the control is off or hidden. */
  readonly wire: () => Record<string, unknown> | undefined;
}

/** The choices of the conversations seen in this page load. */
const kept = new Map<string, RunChoice>();

const sameTool = (a: SensitiveToolRef, b: SensitiveToolRef): boolean => a.toolkit === b.toolkit && a.tool === b.tool;

/** The grant as it goes on the next send: only the named tools that the agent still has. */
export function grantForSend(choice: RunChoice, tools: readonly SensitiveToolRef[] | undefined, grantId: string): Record<string, unknown> | undefined {
  const grant = grantToSend(choice, 'cloud');
  if (grant === null) return undefined;
  const named = grant.sensitive_tools.filter((tool) => tools?.some((option) => sameTool(option, tool)) === true);
  const result = toCloudGrant({ ...grant, sensitive_tools: named }, grantId);
  return result.ok ? { ...result.grant } : undefined;
}

export function useChatBoxUnattended(conversationKey: string | undefined, sensitiveTools: readonly SensitiveToolRef[] | undefined, offered = true): ChatBoxUnattended {
  const grantsOff = useGrantsTurnedOff();
  const [choice, setChoiceState] = useState<RunChoice>(() => (conversationKey === undefined ? undefined : kept.get(conversationKey)) ?? defaultChoice());
  const [shownKey, setShownKey] = useState(conversationKey);
  if (conversationKey !== shownKey) {
    setShownKey(conversationKey);
    if (shownKey !== undefined) setChoiceState((conversationKey === undefined ? undefined : kept.get(conversationKey)) ?? defaultChoice());
    else if (conversationKey !== undefined) kept.set(conversationKey, choice);
  }
  const shown = offered && !grantsOff;
  const setChoice = useCallback(
    (next: RunChoice) => {
      setChoiceState(next);
      if (shownKey !== undefined) kept.set(shownKey, next);
    },
    [shownKey],
  );
  const latest = useStableRef({ choice, shown, sensitiveTools });
  const wire = useCallback(() => {
    const current = latest.current;
    return current.shown && !grantsTurnedOff() ? grantForSend(current.choice, current.sensitiveTools, newGrantId()) : undefined;
  }, [latest]);
  return { shown, choice, setChoice, sensitiveTools, wire };
}
