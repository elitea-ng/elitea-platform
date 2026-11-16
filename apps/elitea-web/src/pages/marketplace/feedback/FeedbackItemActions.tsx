/**
 * The steps on one item (ADR-0051 decision 5). The publisher replies once and
 * flags; a platform administrator flags, unflags, hides and unhides. A reply, a
 * flag and a hide each take a short text in a small form that opens under the
 * item, and every step is a button a keyboard reaches.
 */
import { useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import TextField from '@mui/material/TextField';

import type { MarketplaceFeedbackItem } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import type { ActionRequest } from './useListingFeedback';

export interface ItemRights {
  readonly canReply: boolean;
  readonly canFlag: boolean;
  readonly canModerate: boolean;
}

type Form = 'reply' | 'flag' | 'hide';

function formLabel(form: Form): string {
  switch (form) {
    case 'reply':
      return t('marketplace.feedback.replyLabel', 'Your reply');
    case 'flag':
      return t('marketplace.feedback.flagLabel', 'Why flag it? (optional)');
    case 'hide':
      return t('marketplace.feedback.hideLabel', 'Why hide it? (optional)');
  }
}

function formSubmit(form: Form): string {
  switch (form) {
    case 'reply':
      return t('marketplace.feedback.sendReply', 'Send reply');
    case 'flag':
      return t('marketplace.feedback.confirmFlag', 'Flag');
    case 'hide':
      return t('marketplace.feedback.confirmHide', 'Hide');
  }
}

function StepForm({
  form,
  busy,
  onSubmit,
  onCancel,
}: {
  readonly form: Form;
  readonly busy: boolean;
  readonly onSubmit: (text: string) => void;
  readonly onCancel: () => void;
}): ReactNode {
  const [text, setText] = useState('');
  const required = form === 'reply';
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1, maxWidth: '32rem' }}>
      <TextField
        size="small"
        multiline
        minRows={2}
        label={formLabel(form)}
        value={text}
        onChange={(event) => setText(event.target.value)}
        slotProps={{ htmlInput: { maxLength: form === 'reply' ? 2000 : 500 } }}
      />
      <Box sx={{ display: 'flex', gap: 1 }}>
        <Button
          size="small"
          variant="contained"
          disabled={busy || (required && text.trim() === '')}
          onClick={() => onSubmit(text)}
          data-testid={`marketplace-feedback-submit-${form}`}
        >
          {formSubmit(form)}
        </Button>
        <Button size="small" onClick={onCancel}>
          {t('marketplace.feedback.cancel', 'Cancel')}
        </Button>
      </Box>
    </Box>
  );
}

interface Available {
  readonly reply: boolean;
  readonly flag: boolean;
  readonly unflag: boolean;
  readonly hide: boolean;
  readonly unhide: boolean;
}

/** Which steps the caller may take on this item now. */
function availableSteps(item: MarketplaceFeedbackItem, rights: ItemRights): Available {
  const hidden = item.state === 'hidden';
  return {
    reply: rights.canReply && item.reply === null && !hidden,
    flag: rights.canFlag && !item.flagged,
    unflag: rights.canFlag && item.flagged,
    hide: rights.canModerate && !hidden,
    unhide: rights.canModerate && hidden,
  };
}

function StepButtons({
  itemId,
  available,
  busy,
  onForm,
  onAction,
}: {
  readonly itemId: string;
  readonly available: Available;
  readonly busy: boolean;
  readonly onForm: (form: Form) => void;
  readonly onAction: (request: ActionRequest) => void;
}): ReactNode {
  return (
    <Box sx={{ display: 'flex', gap: 1, flexWrap: 'wrap' }}>
      {available.reply && (
        <Button size="small" onClick={() => onForm('reply')} data-testid={`marketplace-feedback-reply-open-${itemId}`}>
          {t('marketplace.feedback.reply', 'Reply')}
        </Button>
      )}
      {available.flag && (
        <Button size="small" onClick={() => onForm('flag')} data-testid={`marketplace-feedback-flag-${itemId}`}>
          {t('marketplace.feedback.flag', 'Flag')}
        </Button>
      )}
      {available.unflag && (
        <Button
          size="small"
          disabled={busy}
          onClick={() => onAction({ feedbackId: itemId, action: 'unflag' })}
          data-testid={`marketplace-feedback-unflag-${itemId}`}
        >
          {t('marketplace.feedback.unflag', 'Remove flag')}
        </Button>
      )}
      {available.hide && (
        <Button
          size="small"
          color="warning"
          onClick={() => onForm('hide')}
          data-testid={`marketplace-feedback-hide-${itemId}`}
        >
          {t('marketplace.feedback.hide', 'Hide')}
        </Button>
      )}
      {available.unhide && (
        <Button
          size="small"
          disabled={busy}
          onClick={() => onAction({ feedbackId: itemId, action: 'unhide' })}
          data-testid={`marketplace-feedback-unhide-${itemId}`}
        >
          {t('marketplace.feedback.unhide', 'Show again')}
        </Button>
      )}
    </Box>
  );
}

export function FeedbackItemActions({
  item,
  rights,
  busy,
  onAction,
}: {
  readonly item: MarketplaceFeedbackItem;
  readonly rights: ItemRights;
  readonly busy: boolean;
  readonly onAction: (request: ActionRequest) => void;
}): ReactNode {
  const [form, setForm] = useState<Form>();
  const available = availableSteps(item, rights);
  if (!Object.values(available).some(Boolean)) return null;
  const submit = (text: string): void => {
    if (form === undefined) return;
    const body = text.trim();
    if (form === 'reply') onAction({ feedbackId: item.id, action: 'reply', text: body });
    else onAction({ feedbackId: item.id, action: form, ...(body === '' ? {} : { reason: body }) });
    setForm(undefined);
  };
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
      <StepButtons itemId={item.id} available={available} busy={busy} onForm={setForm} onAction={onAction} />
      {form !== undefined && <StepForm form={form} busy={busy} onSubmit={submit} onCancel={() => setForm(undefined)} />}
    </Box>
  );
}
