/**
 * "What the agent saw" (ADR-0050 decision 9, issue 1669): the view Main gave
 * one agent answer, read-only. It names the mode, the window, the digest and
 * every visible message of the conversation by id, author and time, with the
 * reason a message was left out. It never shows a message body, another
 * reader's view (the server answers 404 for one) or anyone's tool calls or
 * thinking. Every value is rendered as React text, never as markup, so a
 * participant's name cannot inject anything.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';
import { useQuery } from '@tanstack/react-query';

import type { MessageContextView, MessageContextViewEntry } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import { fetchMessageContextView, type ContextViewOutcome } from '../../api/contextViewApi';

export interface ContextViewInspectorProps {
  readonly open: boolean;
  readonly projectId: string;
  readonly messageId: string;
  readonly onClose: () => void;
}

function historyText(mode: string): string {
  switch (mode) {
    case 'since_joined':
      return t('features.chatMessages.contextView.history.sinceJoined', 'Since it joined');
    case 'addressed_only':
      return t('features.chatMessages.contextView.history.addressedOnly', 'Only messages addressed to it');
    case 'none':
      return t('features.chatMessages.contextView.history.none', 'None (the current message only)');
    case 'full':
      return t('features.chatMessages.contextView.history.full', 'Everything it may see');
    default:
      return mode;
  }
}

function reasonText(reason: string): string {
  switch (reason) {
    case 'side_talk_of_others':
      return t('features.chatMessages.contextView.reason.sideTalkOfOthers', 'Side talk it is not a party to');
    case 'side_talk_off':
      return t('features.chatMessages.contextView.reason.sideTalkOff', 'Side talk is off for this agent');
    case 'history_none':
      return t('features.chatMessages.contextView.reason.historyNone', 'History is off for this agent');
    case 'before_joined':
      return t('features.chatMessages.contextView.reason.beforeJoined', 'Sent before it joined');
    case 'not_addressed':
      return t('features.chatMessages.contextView.reason.notAddressed', 'Not addressed to it');
    default:
      return reason;
  }
}

function Row({ label, value, testId }: { readonly label: string; readonly value: string; readonly testId: string }): ReactNode {
  return (
    <Box sx={{ display: 'flex', gap: 1 }}>
      <Typography variant="bodySmall" color="text.secondary" sx={{ minWidth: '9rem' }}>
        {label}
      </Typography>
      <Typography variant="bodySmall" data-testid={testId} sx={{ overflowWrap: 'anywhere' }}>
        {value}
      </Typography>
    </Box>
  );
}

function compactionText(view: MessageContextView): string {
  const compaction = view.compaction;
  if (compaction === null) return t('features.chatMessages.contextView.compaction.none', 'Not reported');
  return t('features.chatMessages.contextView.compaction.value', '{{phase}}: {{estimated}} of {{usable}} input tokens (compacts at {{trigger}})', {
    phase: compaction.phase,
    estimated: compaction.estimated_input_tokens,
    usable: compaction.usable_input_tokens,
    trigger: compaction.compaction_trigger_tokens,
  });
}

function EntryRow({ entry }: { readonly entry: MessageContextViewEntry }): ReactNode {
  const author = entry.author_name ?? t('features.chatMessages.contextView.deleted', 'Deleted message');
  const included = entry.reason === null;
  return (
    <Box
      component="li"
      data-testid={included ? 'context-view-included' : 'context-view-excluded'}
      data-message-id={entry.message_id}
      sx={{ display: 'flex', flexDirection: 'column', py: 0.5, opacity: included ? 1 : 0.7 }}
    >
      <Typography variant="bodySmall">
        {author}
        {entry.created_at === null ? '' : ` · ${new Date(entry.created_at).toLocaleString()}`}
      </Typography>
      <Typography variant="labelSmall" color="text.secondary" sx={{ fontFamily: 'monospace' }}>
        {entry.message_id}
      </Typography>
      {!included && entry.reason !== null && (
        <Typography variant="labelSmall" color="text.secondary" data-testid="context-view-reason">
          {t('features.chatMessages.contextView.excludedBecause', 'Left out: {{reason}}', { reason: reasonText(entry.reason) })}
        </Typography>
      )}
    </Box>
  );
}

function ViewBody({ view }: { readonly view: MessageContextView }): ReactNode {
  const included = view.entries.filter((entry) => entry.reason === null);
  const excluded = view.entries.filter((entry) => entry.reason !== null);
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }} data-testid="context-view">
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
        <Row label={t('features.chatMessages.contextView.reader', 'Agent')} value={view.reader_name ?? String(view.reader_participant_id)} testId="context-view-reader" />
        <Row label={t('features.chatMessages.contextView.mode', 'History')} value={historyText(view.history)} testId="context-view-mode" />
        {view.participant_history !== null && (
          <Row
            label={t('features.chatMessages.contextView.participantHistory', 'Set in this conversation')}
            value={historyText(view.participant_history)}
            testId="context-view-participant-history"
          />
        )}
        <Row
          label={t('features.chatMessages.contextView.sideTalk', 'Side talk')}
          value={view.include_side_talk === 'none' ? t('features.chatMessages.contextView.sideTalkNone', 'None') : t('features.chatMessages.contextView.sideTalkParty', 'Side talk it is a party to')}
          testId="context-view-side-talk"
        />
        <Row label={t('features.chatMessages.contextView.window', 'Window')} value={t('features.chatMessages.contextView.windowTokens', '{{count}} tokens', { count: view.window_tokens })} testId="context-view-window" />
        <Row label={t('features.chatMessages.contextView.compactionLabel', 'Context')} value={compactionText(view)} testId="context-view-compaction" />
        <Row label={t('features.chatMessages.contextView.digest', 'Digest')} value={view.digest} testId="context-view-digest" />
      </Box>
      <Typography variant="labelMedium">
        {t('features.chatMessages.contextView.includedTitle', 'Messages it saw ({{count}})', { count: included.length })}
      </Typography>
      <Box component="ul" sx={{ listStyle: 'none', m: 0, p: 0 }}>
        {included.map((entry) => <EntryRow key={entry.message_id} entry={entry} />)}
      </Box>
      {excluded.length > 0 && (
        <>
          <Typography variant="labelMedium">
            {t('features.chatMessages.contextView.excludedTitle', 'Messages it did not see ({{count}})', { count: excluded.length })}
          </Typography>
          <Box component="ul" sx={{ listStyle: 'none', m: 0, p: 0 }}>
            {excluded.map((entry) => <EntryRow key={entry.message_id} entry={entry} />)}
          </Box>
        </>
      )}
    </Box>
  );
}

function OutcomeBody({ outcome }: { readonly outcome: ContextViewOutcome | undefined }): ReactNode {
  if (outcome === undefined) return <CircularProgress size={20} data-testid="context-view-loading" />;
  switch (outcome.kind) {
    case 'view':
      return <ViewBody view={outcome.view} />;
    case 'not_recorded':
      return (
        <Typography variant="bodySmall" data-testid="context-view-not-recorded">
          {t('features.chatMessages.contextView.notRecorded', 'No view was recorded for this answer.')}
        </Typography>
      );
    case 'forbidden':
      return (
        <Typography variant="bodySmall" data-testid="context-view-forbidden">
          {t('features.chatMessages.contextView.forbidden', 'You do not have access to this view.')}
        </Typography>
      );
    case 'unavailable':
      return (
        <Typography variant="bodySmall" data-testid="context-view-unavailable">
          {t('features.chatMessages.contextView.unavailable', 'The view could not be loaded. Try again later.')}
        </Typography>
      );
  }
}

export function ContextViewInspector({ open, projectId, messageId, onClose }: ContextViewInspectorProps): ReactNode {
  const query = useQuery({
    queryKey: ['chat-messages', 'contextView', projectId, messageId],
    queryFn: () => fetchMessageContextView(projectId, messageId),
    enabled: open,
    staleTime: Infinity,
  });
  return (
    <BaseModal
      open={open}
      title={t('features.chatMessages.contextView.title', 'What the agent saw')}
      data-testid="context-view-inspector"
      onClose={onClose}
      content={<OutcomeBody outcome={query.data} />}
      actions={{ cancelText: t('features.chatMessages.contextView.close', 'Close') }}
    />
  );
}
