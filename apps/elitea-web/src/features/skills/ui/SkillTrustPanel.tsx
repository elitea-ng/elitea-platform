/**
 * The trust of a skill written in another project (ADR-0051 amendment 1, A1.6).
 *
 * A skill forked from another project is third-party text. In a chat the model gets
 * it as quoted untrusted data until THIS project trusts the exact version of the text
 * shown here. The person reads it in the editor, then chooses **Trust this version for
 * this project**; any later change to the text is a new version and is untrusted
 * again. **Revoke trust** undoes it. A skill written in this project shows nothing.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { EliteaApiError } from '@/shared/api/generated/mutator';
import { getSkillTrust, revokeSkillTrust, trustSkillRevision } from '@/shared/api/generated/skills/skills';
import type { SkillTrustStatus } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';

const key = (projectId: string, skillId: number) => ['skill-trust', projectId, skillId] as const;

export interface SkillTrustPanelProps {
  readonly projectId: string | undefined;
  readonly skillId: string | undefined;
  /** The person may edit skills: only they can trust or revoke. */
  readonly canEdit: boolean;
  /** The text as saved changed since the panel last read it (the editor saved): re-read. */
  readonly revisionHint?: string | undefined;
}

export function SkillTrustPanel({ projectId, skillId, canEdit, revisionHint }: SkillTrustPanelProps): ReactNode {
  const id = Number(skillId);
  const enabled = projectId !== undefined && Number.isInteger(id) && id > 0;
  const queryClient = useQueryClient();
  const status = useQuery({
    queryKey: [...key(projectId ?? '', id), revisionHint ?? ''],
    queryFn: async (): Promise<SkillTrustStatus> => unwrapBody(await getSkillTrust(projectId ?? '', id)) as SkillTrustStatus,
    enabled,
    retry: false,
  });
  const refresh = (): void => {
    void queryClient.invalidateQueries({ queryKey: key(projectId ?? '', id) });
  };
  const trust = useMutation({
    mutationFn: (revision: string) => trustSkillRevision(projectId ?? '', id, { revision }),
    onSuccess: refresh,
  });
  const revoke = useMutation({ mutationFn: () => revokeSkillTrust(projectId ?? '', id), onSuccess: refresh });

  const data = status.data;
  if (!enabled || data === undefined || !data.foreign) return null;
  const busy = trust.isPending || revoke.isPending;
  const failed = trust.error ?? revoke.error;
  const stale = failed instanceof EliteaApiError && failed.failure.kind === 'http' && failed.failure.status === 409;

  if (data.state === 'trusted') {
    return (
      <Alert
        severity="success"
        data-testid="skill-trust-panel"
        action={
          canEdit ? (
            <Button color="inherit" size="small" disabled={busy} onClick={() => revoke.mutate()} data-testid="skill-trust-revoke">
              {t('skills.trust.revoke', 'Revoke trust')}
            </Button>
          ) : undefined
        }
      >
        {t('skills.trust.trusted', 'This project trusts this version of the text, written in another project. Chats give it to the model as an instruction.')}
      </Alert>
    );
  }
  return (
    <Alert
      severity="warning"
      data-testid="skill-trust-panel"
      action={
        canEdit ? (
          <Button color="inherit" size="small" disabled={busy} onClick={() => trust.mutate(data.revision)} data-testid="skill-trust-grant">
            {t('skills.trust.grant', 'Trust this version for this project')}
          </Button>
        ) : undefined
      }
    >
      {t(
        'skills.trust.untrusted',
        'Written in another project. Until this project trusts this version, chats give its text to the model as quoted data, not as an instruction. Read it, then trust it. Any change to the text needs trust again.',
      )}
      {stale ? ` ${t('skills.trust.stale', 'The text changed since you opened it. Read it again before trusting it.')}` : ''}
      {failed !== null && !stale ? ` ${t('skills.trust.failed', 'The trust could not be changed. Try again.')}` : ''}
    </Alert>
  );
}
