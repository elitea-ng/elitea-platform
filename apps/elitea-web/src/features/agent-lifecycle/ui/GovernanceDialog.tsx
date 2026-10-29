import type { ReactNode } from 'react';
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';

import type { AgentRegistryRecord } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { useAgentRegistry } from '../api/governanceApi';
import { GovernanceAttestationsTab } from './GovernanceAttestationsTab';
import { GovernanceChangeRequestsTab } from './GovernanceChangeRequestsTab';
import { GovernanceRegistryTab } from './GovernanceRegistryTab';
import { certificationStateLabel, formatWhen } from './governanceLabels';

/**
 * The agent's governance (ADR-0040 decisions 1, 3, 5 and 6, #1497), from the
 * editor's ⋮ menu beside Release history.
 *
 * Registry: owners, risk tier, intended use, data classes and the
 * certification. Change requests: release and recertification requests with
 * their release gate, approvals and the approve, reject, withdraw and gate
 * override actions. Attestations: reviews done outside the platform.
 *
 * A lapsed or suspended certification is a banner above the tabs, because
 * under the `suspend` lapse action the agent's runs are already refused with
 * `agent_certification_lapsed` and the reader should not have to find that
 * in a field.
 */
export interface GovernanceDialogProps {
  readonly open: boolean;
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly onClose: () => void;
}

type GovernanceTab = 'registry' | 'changes' | 'attestations';

export function GovernanceDialog({ open, projectId, applicationId, onClose }: GovernanceDialogProps): ReactNode {
  const [tab, setTab] = useState<GovernanceTab>('registry');
  const registry = useAgentRegistry(projectId, applicationId, open);
  return (
    <Dialog
      open={open}
      onClose={onClose}
      maxWidth="md"
      fullWidth
      data-testid="governance-dialog"
    >
      <DialogTitle>{t('features.agentLifecycle.governance.title', 'Governance')}</DialogTitle>
      <DialogContent>
        {registry.data !== undefined && <CertificationBanner record={registry.data} />}
        <Tabs
          value={tab}
          onChange={(_, value: GovernanceTab) => setTab(value)}
        >
          <Tab
            value="registry"
            label={t('features.agentLifecycle.governance.tabRegistry', 'Registry')}
            data-testid="governance-tab-registry"
          />
          <Tab
            value="changes"
            label={t('features.agentLifecycle.governance.tabChanges', 'Change requests')}
            data-testid="governance-tab-changes"
          />
          <Tab
            value="attestations"
            label={t('features.agentLifecycle.governance.tabAttestations', 'Attestations')}
            data-testid="governance-tab-attestations"
          />
        </Tabs>
        <Box sx={{ paddingTop: '1rem' }}>
          {tab === 'registry' && (
            <GovernanceRegistryTab
              projectId={projectId}
              applicationId={applicationId}
              registry={registry}
            />
          )}
          {tab === 'changes' && (
            <GovernanceChangeRequestsTab
              projectId={projectId}
              applicationId={applicationId}
            />
          )}
          {tab === 'attestations' && (
            <GovernanceAttestationsTab
              projectId={projectId}
              applicationId={applicationId}
            />
          )}
        </Box>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>{t('features.agentLifecycle.governance.close', 'Close')}</Button>
      </DialogActions>
    </Dialog>
  );
}

function CertificationBanner({ record }: { readonly record: AgentRegistryRecord }): ReactNode {
  const state = record.certification_state;
  if (state === 'lapsed' || state === 'suspended') {
    return (
      <Alert
        severity="warning"
        sx={bannerSx}
        data-testid="governance-lapsed-banner"
      >
        {state === 'suspended'
          ? t(
              'features.agentLifecycle.governance.suspendedBanner',
              'The certification of this agent lapsed and its runs are refused (agent_certification_lapsed). Start a recertification under Change requests.',
            )
          : t(
              'features.agentLifecycle.governance.lapsedBanner',
              'The certification of this agent lapsed on {{when}}. Start a recertification under Change requests.',
              { when: formatWhen(record.recertify_by) },
            )}
      </Alert>
    );
  }
  if (state === 'due') {
    return (
      <Alert
        severity="info"
        sx={bannerSx}
        data-testid="governance-due-banner"
      >
        {t('features.agentLifecycle.governance.dueBanner', '{{state}}: recertify by {{when}}.', {
          state: certificationStateLabel(state),
          when: formatWhen(record.recertify_by),
        })}
      </Alert>
    );
  }
  return null;
}

const bannerSx = { marginBottom: '0.5rem' };
