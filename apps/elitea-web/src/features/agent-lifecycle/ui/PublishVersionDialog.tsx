import type { ReactNode } from 'react';
import { useEffect, useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import { isValidPublishVersionName, type NormalizedPublishValidation, type PublishCategory } from '../lib/publishValidation';
import { PublishPreparationStep } from './PublishPreparationStep';
import { PublishValidationReport } from './PublishValidationReport';

/**
 * The three-step publish wizard, matching the one production shows
 * (measured read-only on `next.elitea.ai` 2026-09-06: *Preparation →
 * Validation → Publishing*, with a version name, a category and a
 * "I agree with the Publishing Terms" checkbox that gates Continue).
 *
 * The steps are not decoration. The server has two publish modes and the
 * wizard is the only place the difference is visible:
 *
 *  - **Preparation** collects `version_name` and `category`. Both are
 *    server-validated (`^[a-zA-Z0-9._-]+$` and a nine-value whitelist), so
 *    both are checked here too — a refusal the author could have been shown
 *    before they clicked is a refusal that should not have been sent.
 *  - **Validation** calls `publish_validate` and renders its result. A FAIL
 *    stops the wizard here; a PASS/WARN yields a `validation_token`.
 *  - **Publishing** posts the publish with that token, which is what makes
 *    the server skip re-running validation it already ran.
 */

type Step = 'preparation' | 'validation' | 'publishing';

export interface PublishVersionDialogProps {
  readonly open: boolean;
  readonly validation: NormalizedPublishValidation | undefined;
  readonly isValidating: boolean;
  readonly isPublishing: boolean;
  /** Set when a request failed for a reason that is not a validation finding. */
  readonly error: string | undefined;
  readonly onClose: () => void;
  readonly onValidate: (versionName: string, category: PublishCategory | '') => void;
  readonly onPublish: (versionName: string, category: PublishCategory | '', validationToken: string | undefined) => void;
}

export function PublishVersionDialog({
  open,
  validation,
  isValidating,
  isPublishing,
  error,
  onClose,
  onValidate,
  onPublish,
}: PublishVersionDialogProps): ReactNode {
  const [step, setStep] = useState<Step>('preparation');
  // Empty, not the open version's name. Publish CLONES the version under the
  // name given here, and `application_versions` is UNIQUE on
  // (application_id, name) — seeding the field with the name the version
  // already has produces a `version_name_exists_in_source` 422 on the first
  // click. Production leaves it empty for the same reason.
  const [versionName, setVersionName] = useState('');
  const [category, setCategory] = useState<PublishCategory | ''>('');
  const [agreed, setAgreed] = useState(false);

  // Reopening the dialog must not resume the previous attempt's step: the
  // author closed it, and a wizard that reopens on "Publishing" would publish
  // on one click with a state they cannot see.
  useEffect(() => {
    if (!open) return;
    setStep('preparation');
    setVersionName('');
    setCategory('');
    setAgreed(false);
  }, [open]);

  // The validation result arriving is what advances the wizard. Advancing on
  // the CLICK instead would show an empty Validation step while the request
  // was still in flight, which reads as a pass.
  useEffect(() => {
    if (validation !== undefined && step === 'preparation' && !isValidating) setStep('validation');
  }, [validation, step, isValidating]);

  const nameValid = versionName !== '' && isValidPublishVersionName(versionName);
  const blocked = step === 'validation' && validation?.status === 'FAIL';

  // The guard lives HERE and not on a disabled button: `BaseModal`'s action
  // bar has a `confirming` flag and no `disabled` one, so an unguarded confirm
  // would send an invalid version name to the server and surface its 400 as a
  // failure the author cannot connect to the field they left blank. A FAIL is
  // guarded for the same reason in the other direction — the server would
  // refuse it, and the reader is already looking at why.
  const handleConfirm = (): void => {
    if (step === 'preparation') {
      if (!nameValid || !agreed || isValidating) return;
      onValidate(versionName, category);
      return;
    }
    if (blocked || isPublishing) return;
    setStep('publishing');
    onPublish(versionName, category, validation?.validationToken);
  };

  return (
    <BaseModal
      open={open}
      title={t('features.agentLifecycle.publish.title', 'Publish version')}
      data-testid="publish-version-dialog"
      onClose={onClose}
      onConfirm={handleConfirm}
      actions={{
        confirmText: confirmLabel(step),
        confirming: isValidating || isPublishing,
      }}
      content={
        <Box sx={contentSx}>
          <Typography
            variant="labelSmall"
            data-testid="publish-step"
          >
            {step}
          </Typography>
          {step === 'preparation' ? (
            <PublishPreparationStep
              versionName={versionName}
              onVersionNameChange={setVersionName}
              nameValid={nameValid}
              category={category}
              onCategoryChange={setCategory}
              agreed={agreed}
              onAgreedChange={setAgreed}
            />
          ) : (
            validation !== undefined && <PublishValidationReport validation={validation} />
          )}
          {blocked && (
            <Typography
              role="alert"
              variant="bodySmall"
            >
              {t('features.agentLifecycle.publish.blocked', 'This version cannot be published until validation passes.')}
            </Typography>
          )}
          {error !== undefined && (
            <Typography
              role="alert"
              variant="bodySmall"
              data-testid="publish-error"
            >
              {error}
            </Typography>
          )}
        </Box>
      }
    />
  );
}

/** The action button's copy: the wizard advances on Continue and commits on Publish. */
function confirmLabel(step: Step): string {
  return step === 'preparation'
    ? t('features.agentLifecycle.publish.continue', 'Continue')
    : t('features.agentLifecycle.publish.confirm', 'Publish');
}

const contentSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.75rem', minWidth: '24rem' };
