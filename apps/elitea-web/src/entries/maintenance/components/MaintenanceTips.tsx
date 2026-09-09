import { Box, Link, Typography } from '@mui/material';
import { memo } from 'react';

import MaintenanceTipsContainer from './MaintenanceTipsContainer';
import { t } from '@/shared/i18n';

const TIPS_LINK = '/docs/home/onboarding-tips/';
const DOCS_LINK = '/docs/';

const introText = t(
  'entries.maintenance.tips.intro',
  'While we get things ready behind the scenes, why not explore some helpful resources?',
);
const tipsLinkLabel = t('entries.maintenance.tips.tipsLinkLabel', 'Tips & Features');
const tipsLineSuffix = t('entries.maintenance.tips.tipsLineSuffix', 'to get the most out of Elitea');
const tipsLinePrefix = t('entries.maintenance.tips.tipsLinePrefix', 'Discover our');
const docsLinkLabel = t('entries.maintenance.tips.docsLinkLabel', 'Documentation.');
const docsLinePrefix = t('entries.maintenance.tips.docsLinePrefix', 'Browse our guides in');

const MaintenanceTips = memo(() => {
  return (
    <MaintenanceTipsContainer>
      <Typography component="div" variant="bodyMedium" sx={(theme) => ({ color: theme.vars.palette.text.secondary })}>
        {introText}
      </Typography>
      <Box
        component="ul"
        sx={{
          margin: 0,
          paddingLeft: '1.25rem',
          listStyleType: 'disc',
        }}
      >
        <Typography
          component="li"
          variant="bodyMedium"
          sx={(theme) => ({ color: theme.vars.palette.text.secondary })}
        >
          {tipsLinePrefix}{' '}
          <Link
            href={TIPS_LINK}
            target="_blank"
            rel="noopener"
            sx={(theme) => ({
              color: theme.vars.palette.text.link,
              textDecoration: 'underline',
              '&:visited': {
                color: theme.vars.palette.icon.fill.magicAssistant,
              },
              '&:hover': {
                cursor: 'pointer',
                textDecoration: 'underline',
              },
            })}
          >
            {tipsLinkLabel}
          </Link>{' '}
          {tipsLineSuffix}
        </Typography>
        <Typography
          component="li"
          variant="bodyMedium"
          sx={(theme) => ({ color: theme.vars.palette.text.secondary })}
        >
          {docsLinePrefix}{' '}
          <Link
            href={DOCS_LINK}
            target="_blank"
            rel="noopener"
            sx={(theme) => ({
              color: theme.vars.palette.text.link,
              textDecoration: 'underline',
              '&:visited': {
                color: theme.vars.palette.icon.fill.magicAssistant,
              },
              '&:hover': {
                cursor: 'pointer',
                textDecoration: 'underline',
              },
            })}
          >
            {docsLinkLabel}
          </Link>
        </Typography>
      </Box>
    </MaintenanceTipsContainer>
  );
});

MaintenanceTips.displayName = 'MaintenanceTips';

export default MaintenanceTips;
