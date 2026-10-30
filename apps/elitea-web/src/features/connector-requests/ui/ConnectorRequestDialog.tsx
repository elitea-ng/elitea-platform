/**
 * The "Request connector" form (ADR-0044 decision 1, #1501): the tools the
 * connector will expose with their ADR-0041 categories, the egress hosts it
 * needs, the data classes it touches and why. Sent straight to the
 * approvers. A custom MCP server's tools come from its discovery, with their
 * input schemas: the server pins exactly those, so a tool the server adds
 * or changes later stays hidden until a new request is approved.
 */
import { useState, type ReactNode } from 'react';

import CloseIcon from '@mui/icons-material/Close';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogContentText from '@mui/material/DialogContentText';
import DialogTitle from '@mui/material/DialogTitle';
import FormControlLabel from '@mui/material/FormControlLabel';
import IconButton from '@mui/material/IconButton';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { ConnectorDeclaredTool, ConnectorRequest } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

import {
  TOOL_CATEGORIES,
  describeConnectorRequestError,
  splitList,
  useCreateConnectorRequest,
  type ConnectorTarget,
} from '../api/connectorRequests';

export interface ConnectorRequestDialogProps {
  readonly open: boolean;
  readonly projectId: string | undefined;
  readonly target: ConnectorTarget;
  /** The tools a discovery listed (a custom MCP server's carry their input schema). */
  readonly tools?: readonly ConnectorDeclaredTool[] | undefined;
  readonly onClose: () => void;
  readonly onSubmitted: (request: ConnectorRequest) => void;
}

function targetLabel(target: ConnectorTarget): string {
  switch (target.targetKind) {
    case 'custom_mcp':
      return t('features.connectorRequests.kind.customMcp', 'MCP server {{target}}', { target: target.target });
    case 'prebuilt_mcp':
      return t('features.connectorRequests.kind.prebuiltMcp', 'Pre-built MCP server {{target}}', { target: target.target });
    case 'toolkit_type':
      return t('features.connectorRequests.kind.toolkitType', 'Toolkit type {{target}}', { target: target.target });
  }
}

export function ConnectorRequestDialog(props: ConnectorRequestDialogProps): ReactNode {
  const { open, projectId, target, onClose, onSubmitted } = props;
  const discovered = target.targetKind === 'custom_mcp';
  const [tools, setTools] = useState<ConnectorDeclaredTool[]>(() => [...(props.tools ?? [])]);
  const [newTool, setNewTool] = useState('');
  const [hosts, setHosts] = useState('');
  const [classes, setClasses] = useState('');
  const [justification, setJustification] = useState('');
  const [platformWide, setPlatformWide] = useState(false);
  const create = useCreateConnectorRequest(projectId);

  const setCategory = (index: number, category: ConnectorDeclaredTool['category']): void =>
    setTools((current) => current.map((tool, at) => (at === index ? { ...tool, category } : tool)));
  const removeTool = (index: number): void => setTools((current) => current.filter((_, at) => at !== index));
  const addTool = (): void => {
    const name = newTool.trim();
    if (name === '' || tools.some((tool) => tool.name.toLowerCase() === name.toLowerCase())) return;
    setTools((current) => [...current, { name, category: 'write' }]);
    setNewTool('');
  };
  const submit = (): void =>
    create.mutate(
      {
        target,
        tools,
        egressHosts: splitList(hosts),
        dataClasses: splitList(classes).map((value) => value.toLowerCase()),
        justification: justification.trim(),
        platformWide,
      },
      { onSuccess: onSubmitted },
    );
  const blocked = justification.trim() === '' || (discovered && tools.length === 0) || create.isPending;

  return (
    <Dialog open={open} onClose={onClose} fullWidth maxWidth="md" data-testid="connector-request-dialog">
      <DialogTitle>{t('features.connectorRequests.dialog.title', 'Request connector')}</DialogTitle>
      <DialogContent>
        <DialogContentText sx={introSx}>
          {t(
            'features.connectorRequests.dialog.intro',
            'This platform only allows approved connectors. Describe what {{connector}} will do; a connector approver reviews the request.',
            { connector: targetLabel(target) },
          )}
        </DialogContentText>

        <Typography variant="labelMedium" component="h3">
          {t('features.connectorRequests.dialog.tools', 'Tools and their categories')}
        </Typography>
        {tools.length === 0 && (
          <Typography variant="bodySmall" color="text.secondary">
            {discovered
              ? t('features.connectorRequests.dialog.noDiscoveredTools', 'Load the server’s tools first: only the listed tools can be approved.')
              : t('features.connectorRequests.dialog.noTools', 'No tools declared yet. An undeclared tool counts as write.')}
          </Typography>
        )}
        {tools.map((tool, index) => (
          <Box key={tool.name} sx={toolRowSx} data-testid={`connector-request-tool-${tool.name}`}>
            <Typography variant="bodyMedium" sx={toolNameSx}>{tool.name}</Typography>
            <TextField
              select
              size="small"
              label={t('features.connectorRequests.dialog.category', 'Category')}
              value={tool.category}
              onChange={(event) => setCategory(index, event.target.value as ConnectorDeclaredTool['category'])}
              slotProps={{ htmlInput: { 'data-testid': `connector-request-category-${tool.name}` } }}
              sx={categorySx}
            >
              {TOOL_CATEGORIES.map((category) => (
                <MenuItem key={category} value={category}>{category}</MenuItem>
              ))}
            </TextField>
            <IconButton
              size="small"
              aria-label={t('features.connectorRequests.dialog.removeTool', 'Remove tool')}
              onClick={() => removeTool(index)}
            >
              <CloseIcon fontSize="small" />
            </IconButton>
          </Box>
        ))}
        {!discovered && (
          <Box sx={toolRowSx}>
            <TextField
              size="small"
              label={t('features.connectorRequests.dialog.toolName', 'Tool name')}
              value={newTool}
              onChange={(event) => setNewTool(event.target.value)}
              slotProps={{ htmlInput: { maxLength: 128, 'data-testid': 'connector-request-new-tool' } }}
            />
            <Button size="small" onClick={addTool} disabled={newTool.trim() === ''}>
              {t('features.connectorRequests.dialog.addTool', 'Add tool')}
            </Button>
          </Box>
        )}

        <TextField
          fullWidth
          margin="normal"
          label={t('features.connectorRequests.dialog.egressHosts', 'Egress hosts (comma separated)')}
          helperText={t('features.connectorRequests.dialog.egressHostsHelp', 'Hosts the worker must reach, such as api.example.com or *.example.com:443.')}
          value={hosts}
          onChange={(event) => setHosts(event.target.value)}
          slotProps={{ htmlInput: { 'data-testid': 'connector-request-hosts' } }}
        />
        <TextField
          fullWidth
          margin="normal"
          label={t('features.connectorRequests.dialog.dataClasses', 'Data classes (comma separated)')}
          value={classes}
          onChange={(event) => setClasses(event.target.value)}
          slotProps={{ htmlInput: { 'data-testid': 'connector-request-classes' } }}
        />
        <TextField
          fullWidth
          multiline
          minRows={2}
          margin="normal"
          required
          label={t('features.connectorRequests.dialog.justification', 'Justification')}
          value={justification}
          onChange={(event) => setJustification(event.target.value)}
          slotProps={{ htmlInput: { maxLength: 4000, 'data-testid': 'connector-request-justification' } }}
        />
        <FormControlLabel
          control={<BaseCheckbox checked={platformWide} onChange={(event) => setPlatformWide(event.target.checked)} />}
          label={t('features.connectorRequests.dialog.platformWide', 'Request it for every project')}
        />
        {create.isError && <Alert severity="error">{describeConnectorRequestError(create.error)}</Alert>}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>{t('common.cancel', 'Cancel')}</Button>
        <Button variant="contained" disabled={blocked} onClick={submit} data-testid="connector-request-submit">
          {t('features.connectorRequests.dialog.submit', 'Send request')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

const introSx: SxProps<Theme> = { marginBottom: '1rem' };
const toolRowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.75rem', paddingY: '0.25rem' };
const toolNameSx: SxProps<Theme> = { flex: 1, minWidth: 0, overflowWrap: 'anywhere' };
const categorySx: SxProps<Theme> = { minWidth: '11rem' };
