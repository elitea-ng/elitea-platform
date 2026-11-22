import type { ReactNode } from 'react';
import { Children, memo } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { InfoTooltip } from '@/shared/ui/InfoTooltip';

import { getNodeFieldInfo } from '../../lib/flow-editor/constants/nodeFieldInfo.constants';

export type NodeConfigSectionKind = 'basic' | 'advanced';

export interface NodeConfigSectionProps {
  readonly section: NodeConfigSectionKind;
  readonly defaultExpanded?: boolean | undefined;
  readonly children?: ReactNode;
}

export interface NodeFieldHeadingProps {
  readonly label: string;
  readonly nodeType?: string | undefined;
  readonly fieldKey?: string | undefined;
  readonly infoOverride?: string | undefined;
  readonly required?: boolean | undefined;
}

const accordionSlotSx = {
  accordion: { background: 'transparent' },
  summary: { paddingLeft: 0 },
  details: { padding: 0 },
};

const fieldsSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.75rem',
  width: '100%',
};

const headingRowSx: SxProps<Theme> = {
  display: 'inline-flex',
  alignItems: 'center',
  gap: '0.25rem',
};

/**
 * Field label with an optional `InfoTooltip` resolved from Wave U1's `nodeFieldInfo` constants.
 */
export const NodeFieldHeading = memo(function NodeFieldHeading({
  label,
  nodeType,
  fieldKey,
  infoOverride,
  required = false,
}: NodeFieldHeadingProps): ReactNode {
  const infoText = infoOverride ?? (fieldKey ? getNodeFieldInfo(nodeType, fieldKey) : '');

  return (
    <Box sx={headingRowSx} data-testid="pipeline-node-field-heading">
      <Typography variant="labelSmall" color="text.secondary">
        {required ? `${label} *` : label}
      </Typography>
      {infoText ? <InfoTooltip title={infoText} placement="top" data-testid="pipeline-node-field-info" /> : null}
    </Box>
  );
});

/**
 * Collapsible Basic / Advanced section inside `NodeConfigPanel`.
 */
export const NodeConfigSection = memo(function NodeConfigSection({
  section,
  defaultExpanded,
  children,
}: NodeConfigSectionProps): ReactNode {
  if (Children.toArray(children).length === 0) return null;

  const isBasic = section === 'basic';
  const title = isBasic
    ? t('pipelines.nodeConfigPanel.basicSection', 'Basic')
    : t('pipelines.nodeConfigPanel.advancedSection', 'Advanced');

  return (
    <BasicAccordion
      showMode="left"
      defaultExpanded={defaultExpanded ?? isBasic}
      slotSx={accordionSlotSx}
      data-testid={`pipeline-node-panel-${section}-section`}
      items={[
        {
          title,
          content: <Box sx={fieldsSx}>{children}</Box>,
        },
      ]}
    />
  );
});
