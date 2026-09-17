/**
 * Port of
 * apps/elitea-ui/src/[fsd]/features/mcp/ui/modal/OAuthFormFields.jsx
 * (unit A5). Client ID / Client secret / Scope inputs plus an optional
 * "remember credentials" checkbox, shown conditionally per
 * `McpAuthModal`'s flow-detection logic.
 *
 * A13 (ELITEA-0725, "MCP via Chat Conversation: Secret Field Shows 'New
 * Secret' Shortcut"): the Client Secret input is `shared/ui`'s `SecretField`
 * (mode toggle + saved-secret picker + "Create new secret" shortcut),
 * exactly the #441 fix `features/toolkits/ui/form/ToolBase/
 * ToolBaseProperty.renderers.tsx`'s own `SecretFieldInput` leaf already
 * applies elsewhere — see that component's doc comment and
 * `useSecretFieldOptions()`'s own header for why the hook is called from
 * THIS leaf (mounts on the secret path alone) rather than a parent. Plain
 * `StyledInputEnhancer` remains for Client ID/Scope, which are not secrets.
 */
import type { ReactNode } from 'react';

import FormControlLabel from '@mui/material/FormControlLabel';
import Typography from '@mui/material/Typography';

import { useSecretFieldOptions } from '@/entities/secret';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { InfoTooltip } from '@/shared/ui/InfoTooltip';
import { SecretField } from '@/shared/ui/SecretField';
import { StyledInputEnhancer } from '@/shared/ui/StyledInputEnhancer';

export interface OAuthFormFieldsProps {
  clientId: string;
  clientSecret: string;
  scope: string;
  onClientIdChange: (value: string) => void;
  onClientSecretChange: (value: string) => void;
  onScopeChange: (value: string) => void;
  availableScopes?: readonly string[];
  needClientId?: boolean;
  needSecret?: boolean;
  saveCredentials?: boolean;
  onSaveCredentialsChange: (checked: boolean) => void;
  showSaveCredentials?: boolean;
}

export function OAuthFormFields({
  clientId,
  clientSecret,
  scope,
  onClientIdChange,
  onClientSecretChange,
  onScopeChange,
  availableScopes = [],
  needClientId = false,
  needSecret = false,
  saveCredentials = false,
  onSaveCredentialsChange,
  showSaveCredentials = false,
}: OAuthFormFieldsProps): ReactNode {
  const secrets = useSecretFieldOptions();

  return (
    <>
      {needClientId && (
        <StyledInputEnhancer
          autoComplete="off"
          label={t('mcps.oauthForm.clientIdLabel', 'Client ID')}
          placeholder={t('mcps.oauthForm.clientIdPlaceholder', 'Enter OAuth client ID from the provider')}
          onChange={(event) => onClientIdChange(event.target.value)}
          value={clientId}
          required
        />
      )}
      {needSecret && (
        <SecretField
          value={clientSecret}
          onChange={onClientSecretChange}
          label={t('mcps.oauthForm.clientSecretLabel', 'Client Secret')}
          required
          secrets={secrets}
        />
      )}
      <StyledInputEnhancer
        autoComplete="off"
        label={
          <Typography
            variant="bodyMedium"
            color="text.primary"
            sx={{ display: 'flex', alignItems: 'center' }}
          >
            {t('mcps.oauthForm.scopeLabel', 'Scope (optional)')}
            {availableScopes.length > 0 && (
              <InfoTooltip
                title={t('mcps.oauthForm.scopeTooltip', 'MCP server supports: {{scopes}}.', { scopes: availableScopes.join(', ') })}
                sx={{ display: 'inline-flex', alignItems: 'center', marginLeft: '0.25rem', color: 'text.secondary', cursor: 'default' }}
              />
            )}
          </Typography>
        }
        onChange={(event) => onScopeChange(event.target.value)}
        value={scope}
        placeholder={t('mcps.oauthForm.scopePlaceholder', 'Enter OAuth scopes (space-separated)')}
      />
      {showSaveCredentials && (
        <FormControlLabel
          control={
            <BaseCheckbox
              checked={saveCredentials}
              onChange={(_event, checked) => onSaveCredentialsChange(checked)}
            />
          }
          label={
            <Typography
              variant="bodyMedium"
              color="text.primary"
            >
              {t('mcps.oauthForm.rememberCredentials', 'Remember credentials for this session')}
            </Typography>
          }
          sx={{ marginTop: '0.5rem', marginBottom: '0.5rem' }}
        />
      )}
    </>
  );
}
