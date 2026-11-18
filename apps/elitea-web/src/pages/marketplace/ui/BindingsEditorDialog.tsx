/**
 * Edit the bindings of an installed or forked agent (ADR-0051 decision 3). The
 * definition of an installed agent is read-only, but what its toolkits point to
 * is a project setting, so this dialog stays open to the editor. It reuses the
 * hire wizard's Bindings step and the same project targets. While the hire
 * change request is open the tools stay off, and the dialog says so.
 */
import { useEffect, useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import LinearProgress from '@mui/material/LinearProgress';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { getMarketplaceBindings, setMarketplaceBindings } from '@/shared/api/generated/marketplace/marketplace';
import type { MarketplaceBindingsState } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import { BindingsStep } from '../hire/HireSteps';
import {
  applyChoice,
  bindingsOf,
  choicesOf,
  hireErrorCode,
  unboundRequirements,
  type BindingChoices,
} from '../hire/hireModel';
import { useBindingTargets } from '../hire/useBindingTargets';
import { invalidateMarketplace } from '../invalidate';

export interface BindingsEditorDialogProps {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly projectId: string | undefined;
  readonly applicationId: number;
}

function errorText(error: unknown): string {
  const code = hireErrorCode(error);
  if (code === 'binding_missing')
    return t('marketplace.bindings.error.missing', 'Bind every required toolkit, or choose Disable for now.');
  if (code === 'binding_invalid')
    return t('marketplace.bindings.error.invalid', 'One of the choices is not valid for this agent. Choose again.');
  return t('marketplace.bindings.error.generic', 'The bindings were not saved. Try again.');
}

export function BindingsEditorDialog({
  open,
  onClose,
  projectId,
  applicationId,
}: BindingsEditorDialogProps): ReactNode {
  const queryClient = useQueryClient();
  const targets = useBindingTargets(projectId, open);
  const [choices, setChoices] = useState<BindingChoices>({});
  const [saved, setSaved] = useState<MarketplaceBindingsState>();
  const state = useQuery({
    queryKey: ['marketplace', 'bindings', projectId, applicationId],
    enabled: open && projectId !== undefined,
    queryFn: async () =>
      unwrapBody(await getMarketplaceBindings(projectId ?? '', applicationId)) as MarketplaceBindingsState,
  });
  const loaded = state.data;
  // The choices start from what the server holds each time the dialog opens and the read arrives.
  useEffect(() => {
    if (open && loaded !== undefined) {
      setChoices(choicesOf(loaded.bindings));
      setSaved(undefined);
    }
  }, [open, loaded]);

  const current = saved ?? loaded;
  const save = useMutation({
    mutationFn: async () => {
      const requirements = current?.requirements;
      if (requirements === undefined) throw new Error('The bindings did not load');
      const response = await setMarketplaceBindings(projectId ?? '', applicationId, {
        bindings: bindingsOf(requirements, choices),
      });
      return unwrapBody(response) as MarketplaceBindingsState;
    },
    onSuccess: (next) => {
      setSaved(next);
      invalidateMarketplace(queryClient);
    },
  });
  const blocked = current === undefined || unboundRequirements(current.requirements, choices).length > 0;

  return (
    <BaseModal
      open={open}
      onClose={onClose}
      onConfirm={() => !save.isPending && !blocked && save.mutate()}
      title={t('marketplace.bindings.title', 'Bindings')}
      data-testid="bindings-editor"
      actions={{ confirmText: t('marketplace.bindings.save', 'Save bindings'), confirming: save.isPending || blocked }}
      content={
        current === undefined ? (
          <LinearProgress />
        ) : (
          <>
            <BindingsStep
              requirements={current.requirements}
              choices={choices}
              optionsFor={targets.optionsFor}
              onChoose={(key, value) => setChoices((now) => applyChoice(now, key, value))}
            />
            {current.awaiting_approval && (
              <Alert severity="info" data-testid="bindings-awaiting">
                {t('marketplace.bindings.awaiting', 'The tools stay off until a person approves the hire.')}
              </Alert>
            )}
            {saved !== undefined && !save.isPending && (
              <Alert severity="success" data-testid="bindings-saved">
                {t('marketplace.bindings.saved', 'The bindings were saved.')}
              </Alert>
            )}
            {save.isError && (
              <Alert severity="error" data-testid="bindings-error">
                {errorText(save.error)}
              </Alert>
            )}
          </>
        )
      }
    />
  );
}
