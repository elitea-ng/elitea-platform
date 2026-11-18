/**
 * The publish dialog: name the version, pick a category, read the gate's
 * report, publish.
 *
 * Ported from the reference's `PublishWizardModal` steps against this app's
 * `BaseModal`. Two things it does NOT do, both because the server does not:
 *
 *  - it never offers to "publish anyway" past a FAIL. The gate's criticals are
 *    the server's refusal, and a button that sends the request regardless would
 *    be a button whose only outcome is a 400.
 *  - it does not claim an AI reviewed the skill. `ai_validation_available`
 *    comes back false from this service, and the dialog says which checks ran
 *    rather than implying a review that did not happen.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import FormControl from '@mui/material/FormControl';
import InputLabel from '@mui/material/InputLabel';
import LinearProgress from '@mui/material/LinearProgress';
import MenuItem from '@mui/material/MenuItem';
import Select from '@mui/material/Select';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';

import type { SkillPublishingState } from '../model/useSkillPublishing';
import type { SkillValidationIssue } from '../model/publishTypes';

function IssueList({
  title,
  issues,
  severity,
}: {
  readonly title: string;
  readonly issues: readonly SkillValidationIssue[];
  readonly severity: 'error' | 'warning';
}): ReactNode {
  if (issues.length === 0) return null;
  return (
    <Alert severity={severity} sx={{ mt: 1 }}>
      <Typography variant="labelMedium">{title}</Typography>
      <Box component="ul" sx={{ pl: 2, m: 0 }}>
        {issues.map((issue, index) => (
          <li key={`${issue.field ?? 'issue'}-${index}`}>
            <Typography variant="bodySmall">
              {issue.issue ?? ''}
              {issue.fix ? ` — ${issue.fix}` : ''}
            </Typography>
          </li>
        ))}
      </Box>
    </Alert>
  );
}

function ValidationReport({ state }: { readonly state: SkillPublishingState }): ReactNode {
  if (state.isValidating) {
    return (
      <Box sx={{ mt: 2 }}>
        <LinearProgress />
        <Typography variant="bodySmall" color="text.secondary">
          {t('skills.publish.validating', 'Running the pre-publish checks…')}
        </Typography>
      </Box>
    );
  }
  if (!state.report) return null;
  return (
    <Box sx={{ mt: 2 }} data-testid="skill-publish-report">
      <Typography variant="bodyMedium">{state.report.summary}</Typography>
      <IssueList
        title={t('skills.publish.criticals', 'Blocking issues')}
        issues={state.report.critical_issues}
        severity="error"
      />
      <IssueList title={t('skills.publish.warnings', 'Warnings')} issues={state.report.warnings} severity="warning" />
      {!state.report.ai_validation_available && (
        <Typography variant="bodySmall" color="text.secondary" sx={{ display: 'block', mt: 1 }}>
          {t(
            'skills.publish.deterministicOnly',
            'Deterministic checks only — this deployment runs no AI review before publishing.',
          )}
        </Typography>
      )}
    </Box>
  );
}

function PublishActions({ state }: { readonly state: SkillPublishingState }): ReactNode {
  const nameReady = state.versionName.trim().length > 0 && state.versionNameError === undefined;
  const failed = state.report?.status === 'FAIL';
  return (
    <>
      <BaseBtn variant="secondary" onClick={state.close}>
        {t('shared.ui.baseModal.cancel', 'Cancel')}
      </BaseBtn>
      {state.step === 'preparation' || state.report === undefined ? (
        <BaseBtn variant="contained" disabled={!nameReady || state.isValidating} onClick={() => void state.validate()}>
          {t('skills.publish.continue', 'Continue')}
        </BaseBtn>
      ) : (
        <BaseBtn
          variant="contained"
          // `nameReady` too, not just the verdict: the field stays editable
          // after validation, so a name the client has ALREADY judged invalid
          // (or colliding) would otherwise be posted and refused by the server
          // with a 400 this side had already computed.
          disabled={failed || !nameReady || state.isPublishing}
          onClick={() => void state.publish()}
        >
          {t('skills.publish.confirm', 'Publish')}
        </BaseBtn>
      )}
    </>
  );
}

export function PublishSkillModal({ state }: { readonly state: SkillPublishingState }): ReactNode {
  return (
    <BaseModal
      open={state.isOpen}
      onClose={state.close}
      title={t('skills.publish.title', 'Publish skill')}
      data-testid="skill-publish-modal"
      actions={{ node: <PublishActions state={state} /> }}
      content={
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2, minWidth: 420 }}>
          <Typography variant="bodySmall" color="text.secondary">
            {t(
              'skills.publish.explain',
              'Publishing snapshots this version under a new name and copies it into the public skill catalog.',
            )}
          </Typography>
          <TextField
            size="small"
            label={t('skills.publish.versionName', 'Published version name')}
            value={state.versionName}
            error={state.versionNameError !== undefined}
            helperText={state.versionNameError ?? ' '}
            onChange={(event) => state.setVersionName(event.target.value)}
            slotProps={{ htmlInput: { 'aria-label': 'Published version name' } }}
          />
          <FormControl size="small">
            <InputLabel id="skill-publish-category">{t('skills.publish.category', 'Category')}</InputLabel>
            <Select
              labelId="skill-publish-category"
              label={t('skills.publish.category', 'Category')}
              value={state.category}
              onChange={(event) => state.setCategory(event.target.value)}
            >
              {state.categories.map((category) => (
                <MenuItem key={category.name} value={category.name}>
                  {category.name}
                </MenuItem>
              ))}
            </Select>
          </FormControl>
          <TextField
            size="small"
            multiline
            minRows={3}
            label={t('skills.publish.releaseNotes', 'Release notes (optional)')}
            helperText={t('skills.publish.releaseNotesHelp', 'Say what changed. The listing page shows this text.')}
            value={state.releaseNotes}
            onChange={(event) => state.setReleaseNotes(event.target.value)}
            slotProps={{ htmlInput: { maxLength: 4000, 'aria-label': 'Release notes' } }}
            data-testid="skill-publish-release-notes"
          />
          {state.error !== undefined && (
            <Alert severity="error" data-testid="skill-publish-error">
              {state.error}
            </Alert>
          )}
          <ValidationReport state={state} />
          {state.isPublishing && <LinearProgress />}
        </Box>
      }
    />
  );
}
