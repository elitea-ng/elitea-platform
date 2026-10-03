/**
 * The sample shell one scheme renders (ADR-0024 WP9): a mock browser tab
 * with the favicon, an app bar with the logo, a sidebar fragment, a heading,
 * a card of body text, the two button kinds, a chip, an input, an alert and
 * a chat turn. Every surface reads `theme.vars` of the SCOPED preview theme
 * the parent provides, so the derivation, not this file, decides a colour.
 */
import Alert from '@mui/material/Alert';
import AppBar from '@mui/material/AppBar';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Paper from '@mui/material/Paper';
import TextField from '@mui/material/TextField';
import Toolbar from '@mui/material/Toolbar';
import Typography from '@mui/material/Typography';

import type { BrandPack } from '@/shared/brand';
import { t } from '@/shared/i18n';

import { displayUrlFor, type LoadedAssets } from '../lib/assets';

interface SurfaceProps {
  readonly pack: BrandPack;
  readonly assets: LoadedAssets;
}

function MockTab({ pack, assets }: SurfaceProps) {
  const favicon = displayUrlFor(pack, 'favicon', assets);
  return (
    <Box
      sx={(theme) => ({
        display: 'flex',
        alignItems: 'center',
        gap: 1,
        padding: '0.375rem 0.75rem',
        backgroundColor: theme.vars.palette.background.paper,
        borderBottom: `0.0625rem solid ${theme.vars.palette.border.lines}`,
      })}
    >
      {favicon === undefined ? (
        <Box
          aria-hidden
          sx={(theme) => ({
            width: '1rem',
            height: '1rem',
            borderRadius: theme.vars.shape.radiusSm,
            backgroundColor: theme.vars.palette.primary.main,
          })}
        />
      ) : (
        <Box component="img" src={favicon} alt={t('entries.brandPreview.surface.faviconAlt', 'favicon')} sx={{ width: '1rem', height: '1rem' }} />
      )}
      <Typography variant="labelSmall" component="span" data-testid="brand-preview-tab-title">
        {pack.product.name}
      </Typography>
    </Box>
  );
}

function Logo({ pack, assets }: SurfaceProps) {
  const logo = displayUrlFor(pack, 'logoFull', assets) ?? displayUrlFor(pack, 'logoMark', assets);
  if (logo === undefined) {
    return (
      <Typography variant="headingSmall" component="span" data-testid="brand-preview-logo-text">
        {pack.product.name}
      </Typography>
    );
  }
  return <Box component="img" src={logo} alt={pack.product.name} sx={{ height: '1.5rem', maxWidth: '12rem' }} />;
}

function Sidebar() {
  const items = [
    { key: 'chats', label: t('entries.brandPreview.surface.navChats', 'Chats'), selected: true },
    { key: 'agents', label: t('entries.brandPreview.surface.navAgents', 'Agents'), selected: false },
    { key: 'pipelines', label: t('entries.brandPreview.surface.navPipelines', 'Pipelines'), selected: false },
  ];
  return (
    <Box
      component="nav"
      aria-label={t('entries.brandPreview.surface.sidebar', 'Sample sidebar')}
      sx={(theme) => ({
        width: '9rem',
        flex: '0 0 auto',
        display: 'flex',
        flexDirection: 'column',
        gap: 0.5,
        padding: 1,
        borderRight: `0.0625rem solid ${theme.vars.palette.border.lines}`,
      })}
    >
      {items.map((item) => (
        <Box
          key={item.key}
          sx={(theme) => ({
            padding: '0.375rem 0.75rem',
            borderRadius: theme.vars.shape.radiusSm,
            backgroundColor: item.selected
              ? theme.vars.palette.background.button.drawerMenu.selected
              : theme.vars.palette.background.button.drawerMenu.default,
          })}
        >
          <Typography variant="labelMedium" component="span">
            {item.label}
          </Typography>
        </Box>
      ))}
    </Box>
  );
}

function ChatTurns() {
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
      <Paper
        elevation={0}
        sx={(theme) => ({
          alignSelf: 'flex-end',
          maxWidth: '80%',
          padding: 1,
          borderRadius: theme.vars.shape.radiusMd,
          backgroundColor: theme.vars.palette.background.button.default,
        })}
      >
        <Typography variant="bodySmall">{t('entries.brandPreview.surface.userTurn', 'Summarise the release notes for me.')}</Typography>
      </Paper>
      <Paper
        elevation={0}
        sx={(theme) => ({
          alignSelf: 'flex-start',
          maxWidth: '80%',
          padding: 1,
          borderRadius: theme.vars.shape.radiusMd,
          backgroundColor: theme.vars.palette.background.aiAnswerBkg,
          border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
        })}
      >
        <Typography variant="bodySmall">
          {t('entries.brandPreview.surface.assistantTurn', 'Three changes landed: branded e-mail, a login page that carries the pack, and this previewer.')}
        </Typography>
      </Paper>
    </Box>
  );
}

function MainColumn({ pack }: { readonly pack: BrandPack }) {
  return (
    <Box sx={{ flex: '1 1 auto', minWidth: 0, padding: 2, display: 'flex', flexDirection: 'column', gap: 1.5 }}>
      <Typography variant="headingMedium" component="h4">
        {pack.product.tagline ?? t('entries.brandPreview.surface.heading', 'A heading in the brand type')}
      </Typography>
      <Paper sx={{ padding: 1.5, display: 'flex', flexDirection: 'column', gap: 1 }}>
        <Typography variant="bodyMedium">
          {t('entries.brandPreview.surface.body', 'Body text on a card. Links, chips and buttons take the derived accent.')}
        </Typography>
        <Box sx={{ display: 'flex', gap: 1, flexWrap: 'wrap', alignItems: 'center' }}>
          <Button variant="elitea" color="primary" size="small">
            {t('entries.brandPreview.surface.primaryAction', 'Primary action')}
          </Button>
          <Button variant="secondary" size="small">
            {t('entries.brandPreview.surface.secondaryAction', 'Secondary')}
          </Button>
          <Chip size="small" label={pack.product.shortName} />
        </Box>
        <TextField
          size="small"
          label={t('entries.brandPreview.surface.inputLabel', 'Conversation name')}
          defaultValue={t('entries.brandPreview.surface.inputValue', 'Weekly release digest')}
        />
      </Paper>
      <Alert severity="info">{t('entries.brandPreview.surface.alert', 'Your workspace was updated a moment ago.')}</Alert>
      <ChatTurns />
    </Box>
  );
}

/** The shell. Rendered inside the scoped `ThemeProvider` and the scheme attribute box. */
export function SampleSurface({ pack, assets }: SurfaceProps) {
  return (
    <Box
      sx={(theme) => ({
        borderRadius: theme.vars.shape.radiusMd,
        overflow: 'hidden',
        border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
        backgroundColor: theme.vars.palette.background.default,
        color: theme.vars.palette.text.primary,
        fontFamily: theme.typography.fontFamily,
      })}
    >
      <MockTab pack={pack} assets={assets} />
      <AppBar position="static" elevation={0}>
        <Toolbar variant="dense" sx={{ gap: 1.5 }}>
          <Logo pack={pack} assets={assets} />
        </Toolbar>
      </AppBar>
      <Box sx={{ display: 'flex' }}>
        <Sidebar />
        <MainColumn pack={pack} />
      </Box>
    </Box>
  );
}
