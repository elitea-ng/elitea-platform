/**
 * The `oauth_app_select` field of the Microsoft credentials (Outlook, Teams, Microsoft
 * Graph, Dataverse, SharePoint): "Default app" or "Custom app" (ADR-0057 decision 5).
 *
 * A Default app is an Entra app an administrator registered in Admin › Configuration ›
 * Microsoft apps. The credential then stores only the app's id (`oauth_app_id`); the
 * platform resolves the client, secret, authority, permission ceiling and refresh
 * setting at every use, so the secret never reaches this browser. Custom app clears
 * the id, and the credential keeps its own client ID and secret.
 *
 * ## An empty registry shows only Custom app
 *
 * Nothing ships enabled. With no registered app that serves this type and project, the
 * field renders nothing at all, and the form is exactly what it was before the
 * registry existed. When the administrator also turned Custom app off
 * (`microsoft.custom_apps`) and an app exists, the Custom option is not offered.
 *
 * The field needs the credential type and the project. A form that does not supply the
 * type gets nothing, never a guess.
 */
import type { ReactNode } from 'react';

import { useMicrosoftAppOffers } from '../../../api/useMicrosoftAppOffers';
import { t } from '@/shared/i18n';

import { EnumSelectField } from './ToolBaseProperty.renderers';

/** The select's value for Custom app; the credential stores no `oauth_app_id` then. */
const CUSTOM_APP_VALUE = '';

export interface OauthAppSelectFieldProps {
  readonly label: string;
  readonly description: string | undefined;
  readonly required: boolean;
  readonly credentialType: string | undefined;
  readonly projectId: string | number | undefined;
  /** The credential's current `oauth_app_id`, or nothing for Custom app. */
  readonly value: string | null | undefined;
  readonly disabled: boolean;
  /** Called with the app's id, or `undefined` for Custom app. */
  readonly onChange: (appId: string | undefined) => void;
}

interface OfferedApp {
  readonly id: string;
  readonly name: string;
}

/** The select's options: Custom app (unless the administrator turned `microsoft.custom_apps` off), the offered apps, and the app a credential names that is no longer offered. */
function appOptions(apps: readonly OfferedApp[], current: string | undefined, customAllowed: boolean) {
  const missing = current !== undefined && !apps.some((app) => app.id === current);
  return [
    ...(customAllowed ? [{ label: t('toolkits.oauthApp.custom', 'Custom app'), value: CUSTOM_APP_VALUE }] : []),
    ...apps.map((app) => ({ label: app.name, value: app.id })),
    ...(missing ? [{ label: t('toolkits.oauthApp.unavailable', 'Default app (not available)'), value: current }] : []),
  ];
}

export function OauthAppSelectField({
  label,
  description,
  required,
  credentialType,
  projectId,
  value,
  disabled,
  onChange,
}: OauthAppSelectFieldProps): ReactNode {
  const offers = useMicrosoftAppOffers({ credentialType, projectId });
  const apps = offers.data?.apps ?? [];
  // The credential names an app that is no longer offered (disabled, or the project
  // lost access): keep it visible so the form says so, instead of silently showing
  // Custom app for a credential that would still resolve the app.
  const current = typeof value === 'string' && value !== '' ? value : undefined;
  if (apps.length === 0 && current === undefined) return null;
  return (
    <EnumSelectField
      label={label}
      description={description}
      required={required}
      value={current ?? CUSTOM_APP_VALUE}
      options={appOptions(apps, current, offers.data?.custom_app_allowed !== false)}
      disabled={disabled}
      onChange={(next) => {
        onChange(next === CUSTOM_APP_VALUE ? undefined : next);
      }}
    />
  );
}

/** What `renderOauthAppSelect` reads of the dispatcher's field context. Structural, so this file does not import the dispatcher (which imports this one). */
interface OauthAppRenderContext {
  readonly key: string;
  readonly schema: { readonly description?: string | undefined };
  readonly label: string;
  readonly required: boolean;
  readonly settings: Readonly<Record<string, unknown>>;
  readonly effectiveDisabled: boolean;
  readonly credentialContext: { readonly credentialType?: string | undefined; readonly specifiedProjectId?: string | number | undefined } | undefined;
  readonly buildEditFieldPath: (fieldKey: string) => string;
  readonly editField: (fieldPath: string, value: never) => void;
}

/** The Microsoft credentials' Default app / Custom app select (ADR-0057 decision 5). A component, because it reads the registry's offers. */
export function renderOauthAppSelect(ctx: OauthAppRenderContext): ReactNode {
  return (
    <OauthAppSelectField
      label={ctx.label}
      description={ctx.schema.description}
      required={ctx.required}
      credentialType={ctx.credentialContext?.credentialType}
      projectId={ctx.credentialContext?.specifiedProjectId}
      value={ctx.settings[ctx.key] as string | null | undefined}
      disabled={ctx.effectiveDisabled}
      onChange={(appId) => {
        ctx.editField(ctx.buildEditFieldPath(ctx.key), appId as never);
      }}
    />
  );
}
