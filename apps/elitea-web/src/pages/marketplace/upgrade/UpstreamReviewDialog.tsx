/**
 * Review the upstream update of a fork (ADR-0051 decision 4). The view has up
 * to three sides: base (the version the fork came from), theirs (the new listing
 * version) and ours (the fork's current draft). Each field is kept as theirs, kept
 * as ours or edited; the conflicting fields come first and cannot be skipped. The
 * result is a new draft and a change request, never a silent update. A fork with
 * no origin version has no base, so the view is a two-way diff.
 */
import { useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import LinearProgress from '@mui/material/LinearProgress';
import Radio from '@mui/material/Radio';
import RadioGroup from '@mui/material/RadioGroup';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import {
  getMarketplaceUpstreamReview,
  proposeMarketplaceUpstreamMerge,
} from '@/shared/api/generated/marketplace/marketplace';
import type {
  MarketplaceMergeField,
  MarketplaceUpstreamResult,
  MarketplaceUpstreamReview,
} from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';
import { ChangeList } from '@/shared/ui/ChangeList';

import { invalidateMarketplace } from '../invalidate';

import {
  blockingFields,
  mergeBlocked,
  fieldText,
  resolutionsOf,
  statusText,
  upgradeErrorMessage,
  wideningText,
  type FieldPick,
  type FieldPicks,
} from './upgradeModel';

export interface UpstreamReviewDialogProps {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly projectId: string | undefined;
  readonly applicationId: number;
}

function Side({
  label,
  value,
  testId,
}: {
  readonly label: string;
  readonly value: unknown;
  readonly testId: string;
}): ReactNode {
  const text = fieldText(value);
  return (
    <Box sx={{ flex: '1 1 0', minWidth: 0 }} data-testid={testId}>
      <Typography variant="labelSmall" color="text.secondary" component="p">
        {label}
      </Typography>
      <Typography variant="bodySmall" sx={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word', fontFamily: 'monospace' }}>
        {text === '' ? t('marketplace.upstream.empty', '(empty)') : text}
      </Typography>
    </Box>
  );
}

interface FieldRowProps {
  readonly field: MarketplaceMergeField;
  readonly threeWay: boolean;
  readonly pick: FieldPick | undefined;
  readonly invalid: boolean;
  readonly onPick: (pick: FieldPick | undefined) => void;
}

function FieldRow({ field, threeWay, pick, invalid, onPick }: FieldRowProps): ReactNode {
  const choice = pick?.choice ?? (field.status === 'conflict' ? '' : field.default);
  return (
    <Box
      component="li"
      sx={{
        display: 'flex',
        flexDirection: 'column',
        gap: 1,
        paddingBottom: 1.5,
        borderBottom: 1,
        borderColor: 'divider',
      }}
      data-testid={`upstream-field-${field.key}`}
    >
      <Typography variant="labelMedium" component="h4">
        {field.label}{' '}
        <Typography
          component="span"
          variant="bodySmall"
          color={field.status === 'conflict' ? 'error.main' : 'text.secondary'}
        >
          {statusText(field.status)}
        </Typography>
      </Typography>
      <Box sx={{ display: 'flex', gap: 1.5, flexWrap: 'wrap' }}>
        {threeWay && (
          <Side
            label={t('marketplace.upstream.base', 'Base')}
            value={field.base}
            testId={`upstream-base-${field.key}`}
          />
        )}
        <Side
          label={t('marketplace.upstream.theirs', 'Theirs')}
          value={field.theirs}
          testId={`upstream-theirs-${field.key}`}
        />
        <Side label={t('marketplace.upstream.ours', 'Ours')} value={field.ours} testId={`upstream-ours-${field.key}`} />
      </Box>
      <RadioGroup
        row
        value={choice}
        aria-label={t('marketplace.upstream.choose', 'Choose for {{field}}', { field: field.label })}
        onChange={(event) => {
          const next = event.target.value;
          if (next === 'edit')
            onPick({ choice: 'edit', text: fieldText(field.status === 'theirs_only' ? field.theirs : field.ours) });
          else if (next === 'theirs' || next === 'ours') onPick({ choice: next });
        }}
      >
        <FormControlLabel
          value="theirs"
          control={<Radio size="small" />}
          label={t('marketplace.upstream.keepTheirs', 'Keep theirs')}
          data-testid={`upstream-keep-theirs-${field.key}`}
        />
        <FormControlLabel
          value="ours"
          control={<Radio size="small" />}
          label={t('marketplace.upstream.keepOurs', 'Keep ours')}
          data-testid={`upstream-keep-ours-${field.key}`}
        />
        <FormControlLabel
          value="edit"
          control={<Radio size="small" />}
          label={t('marketplace.upstream.edit', 'Edit')}
          data-testid={`upstream-edit-${field.key}`}
        />
      </RadioGroup>
      {pick?.choice === 'edit' && (
        <TextField
          multiline
          minRows={3}
          size="small"
          label={t('marketplace.upstream.editLabel', 'Edited {{field}}', { field: field.label })}
          value={pick.text}
          error={invalid}
          helperText={invalid ? t('marketplace.upstream.invalidJson', 'This field must be valid JSON.') : undefined}
          onChange={(event) => onPick({ choice: 'edit', text: event.target.value })}
        />
      )}
    </Box>
  );
}

function ReviewBody({
  review,
  picks,
  onPick,
}: {
  readonly review: MarketplaceUpstreamReview;
  readonly picks: FieldPicks;
  readonly onPick: (key: string, pick: FieldPick | undefined) => void;
}): ReactNode {
  const threeWay = review.mode === 'three_way';
  const bad = new Set(blockingFields(review, picks));
  const open = review.fields.filter((field) => field.status !== 'same');
  const same = review.fields.filter((field) => field.status === 'same');
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }} data-testid="upstream-review">
      <Typography variant="bodySmall">
        {threeWay
          ? t('marketplace.upstream.threeWay', 'Base is {{base}}, theirs is {{theirs}}, ours is your draft {{ours}}.', {
              base: review.base?.name ?? '',
              theirs: review.theirs.name,
              ours: review.our_version_name,
            })
          : t(
              'marketplace.upstream.twoWay',
              'This fork does not know which version it came from, so you see theirs ({{theirs}}) against ours ({{ours}}).',
              {
                theirs: review.theirs.name,
                ours: review.our_version_name,
              },
            )}
      </Typography>
      {review.theirs.release_notes !== '' && (
        <Typography variant="bodySmall" sx={{ whiteSpace: 'pre-wrap' }}>
          {review.theirs.release_notes}
        </Typography>
      )}
      {review.widening.length > 0 && (
        <Alert severity="info" data-testid="upstream-widening">
          {t(
            'marketplace.upstream.widening',
            'Upstream now also asks for these. Your fork keeps its own tools, so add them by hand if you want them:',
          )}
          <Box component="ul" sx={{ margin: 0, paddingLeft: '1.25rem' }}>
            {review.widening.map((item) => (
              <li key={`${item.kind}:${item.detail}`}>{wideningText(item)}</li>
            ))}
          </Box>
        </Alert>
      )}
      {open.length === 0 && (
        <Typography variant="bodySmall">
          {t('marketplace.upstream.nothing', 'Nothing differs. Merging only records the new version.')}
        </Typography>
      )}
      <Box
        component="ul"
        sx={{ margin: 0, padding: 0, listStyle: 'none', display: 'flex', flexDirection: 'column', gap: 1.5 }}
      >
        {open.map((field) => (
          <FieldRow
            key={field.key}
            field={field}
            threeWay={threeWay}
            pick={picks[field.key]}
            invalid={bad.has(field.key) && picks[field.key] !== undefined}
            onPick={(pick) => onPick(field.key, pick)}
          />
        ))}
      </Box>
      {same.length > 0 && (
        <Typography variant="bodySmall" color="text.secondary">
          {t('marketplace.upstream.sameFields', 'Same on both sides: {{fields}}.', {
            fields: same.map((field) => field.label).join(', '),
          })}
        </Typography>
      )}
      {threeWay && review.changes.length > 0 && (
        <>
          <Typography variant="labelMedium" component="h3">
            {t('marketplace.upstream.changed', 'What upstream changed')}
          </Typography>
          <ChangeList changes={review.changes} />
        </>
      )}
      <Alert severity="info">
        {review.needs_approval
          ? t(
              'marketplace.upstream.needsApproval',
              'The merge becomes a new draft and a change request. A person approves it before the fork moves to this version.',
            )
          : t(
              'marketplace.upstream.oneClick',
              'The merge becomes a new draft. It is recorded as approved by you, and the fork moves to this version.',
            )}
      </Alert>
      {review.open_change_request && (
        <Alert severity="warning">
          {t('marketplace.upstream.open', 'This agent already has an open change request. Finish it before you merge.')}
        </Alert>
      )}
    </Box>
  );
}

function Outcome({
  merge,
  result,
}: {
  readonly merge: { readonly isError: boolean; readonly error: unknown };
  readonly result: MarketplaceUpstreamResult | undefined;
}): ReactNode {
  if (result !== undefined) {
    return (
      <Alert severity="success" data-testid="upstream-result">
        {result.applied
          ? t('marketplace.upstream.applied', 'The merge was saved as a new draft and the fork moved to this version.')
          : t(
              'marketplace.upstream.sent',
              'The merge was saved as a new draft and sent for approval. The fork stays on its version until a person approves.',
            )}
      </Alert>
    );
  }
  if (!merge.isError) return null;
  return (
    <Alert severity="error" data-testid="upstream-merge-error">
      {upgradeErrorMessage(merge.error)}
    </Alert>
  );
}

export function UpstreamReviewDialog({
  open,
  onClose,
  projectId,
  applicationId,
}: UpstreamReviewDialogProps): ReactNode {
  const queryClient = useQueryClient();
  const [picks, setPicks] = useState<FieldPicks>({});
  const [result, setResult] = useState<MarketplaceUpstreamResult>();
  const review = useQuery({
    queryKey: ['marketplace', 'upstream-review', projectId, applicationId],
    enabled: open && projectId !== undefined,
    retry: false,
    queryFn: async ({ signal }) =>
      unwrapBody(
        await getMarketplaceUpstreamReview(projectId ?? '', applicationId, undefined, { signal }),
      ) as MarketplaceUpstreamReview,
  });
  const loaded = review.data;
  const merge = useMutation({
    mutationFn: async () => {
      if (loaded === undefined) throw new Error('The review did not load');
      const response = await proposeMarketplaceUpstreamMerge(projectId ?? '', applicationId, {
        version_id: loaded.theirs.id,
        resolutions: resolutionsOf(loaded, picks),
      });
      return unwrapBody(response) as MarketplaceUpstreamResult;
    },
    onSuccess: (done) => {
      setResult(done);
      invalidateMarketplace(queryClient);
    },
  });
  const blocked = loaded === undefined || mergeBlocked(loaded, picks) || result !== undefined;

  return (
    <BaseModal
      open={open}
      onClose={onClose}
      onConfirm={() => !merge.isPending && !blocked && merge.mutate()}
      title={t('marketplace.upstream.title', 'Review the upstream update')}
      data-testid="upstream-dialog"
      actions={{
        confirmText: t('marketplace.upstream.propose', 'Propose the merge'),
        confirming: merge.isPending || blocked,
        cancelText:
          result === undefined ? t('shared.ui.baseModal.cancel', 'Cancel') : t('marketplace.upgrade.close', 'Close'),
      }}
      content={
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2, minWidth: { xs: 0, sm: 560 } }}>
          {review.isPending && <LinearProgress />}
          {review.isError && (
            <Alert severity="error" data-testid="upstream-error">
              {t('marketplace.upstream.failed', 'The review could not be read. Try again.')}
            </Alert>
          )}
          {loaded !== undefined && (
            <ReviewBody
              review={loaded}
              picks={picks}
              onPick={(key, pick) => setPicks((now) => ({ ...now, [key]: pick }))}
            />
          )}
          <Outcome merge={merge} result={result} />
        </Box>
      }
    />
  );
}
