/**
 * Admin › Configuration › LLM Proxy › Providers & models: the platform model
 * delete confirmation, moved out of `./PlatformModelsPanel.tsx` unchanged so
 * the panel stays under the 400-line cap (R3.4b, issue 2613).
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';

import { t } from '@/shared/i18n';

import { PlatformModelDeleteImpact } from './PlatformDefaultModelCard';
import type { PlatformModel } from './api/adminLlmPlatformModelsApi';


/**
 * The delete confirmation, matching the provider surface's.
 *
 * Deleting a platform model withdraws it from every project at once, and the
 * next call from any of them answers `model_not_found`. That is not an action a
 * single click on a table row should perform — and the providers table beside
 * this one already confirms, so leaving this one immediate would make the more
 * destructive-looking surface the safer one.
 */
export function ConfirmModelDelete({
  model,
  onCancel,
  onConfirm,
}: {
  readonly model: PlatformModel | undefined;
  readonly onCancel: () => void;
  readonly onConfirm: (model: PlatformModel) => void;
}): ReactNode {
  if (model === undefined) return null;
  return (
    <Alert
      severity="warning"
      data-testid="platform-models-confirm-delete"
      action={
        <>
          <Button
            variant="elitea" color="tertiary" size="small" onClick={onCancel}>
            {t('pages.admin.platformModels.cancel', 'Cancel')}
          </Button>
          <Button
            variant="elitea" color="alarm"
            size="small"
                        data-testid="platform-models-confirm-delete-button"
            onClick={() => onConfirm(model)}
          >
            {t('pages.admin.platformModels.delete', 'Delete')}
          </Button>
        </>
      }
    >
      {t(
        'pages.admin.platformModels.confirmDelete',
        'Deleting “{{name}}” withdraws it from every project at once. Calls addressing it then fail with model_not_found.',
        { name: model.elitea_title },
      )}
      <PlatformModelDeleteImpact modelId={model.id} />
    </Alert>
  );
}
