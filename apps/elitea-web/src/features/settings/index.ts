/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 *
 * `src/features/settings/` backs the settings domain's page-level
 * compositions in `src/pages/settings/` (Wave-2 unit A9, relocated here
 * from `src/routes/_shell/settings/` — see that unit's report). One
 * OBJECT BUNDLE per subdomain (same pattern established by
 * `widgets/chat-box`'s `participantSources`/`voiceHooks`/
 * `agentEditorHooks`) keeps the ~26 symbols pages/settings/** actually
 * needs down to a handful of slots — each bundle costs exactly 1 slot
 * regardless of property count.
 */
import { useUsersActions } from './lib/users/useUsersActions';
import { UsersPageContent } from './ui/users/UsersPageContent';

import { ServicePromptsBody } from './ui/system-prompts/ServicePromptsBody';
export type { PromptConfig } from './ui/system-prompts/ServicePrompts.types';

import { SecretsTable } from './ui/secrets/SecretsTable';
import { SecretValueCell } from './ui/secrets/SecretValueCell';
import { useSecretPermissions } from './lib/secrets/useSecretPermissions';


import { ProjectContextBody, ProjectContextToasts } from './ui/project-context/ProjectContextBody';
import { AgentPipelineBuilder } from './ui/project-general/AgentPipelineBuilder';
import { ProjectAIConfigurationSection } from './ui/project-general/ProjectAIConfigurationSection';
import { hasSavedProjectContext } from './lib/project-context/hasSavedContent';
import { ProjectContextEmptyState } from './ui/project-context/ProjectContextEmptyState';
import { ProjectParamsHeader } from './ui/project-context/ProjectParamsHeader';
import { projectContextStyles } from './ui/project-context/ProjectContext.styles';
// Type-only, so it costs no export slot (same as `PromptConfig` above). The
// page that persists an icon needs the shape the dialog emits.
export type { SelectedProjectIcon } from './ui/project-context/ProjectIconDialog';

import { formatLastLogin, selectLastLogin } from './lib/profile/lastLogin';
import { useDefaultModel } from './lib/profile/useDefaultModel';
import { ProfileFormContent } from './ui/profile/ProfileFormContent';
import { ProfileIdentity } from './ui/profile/ProfileIdentity';
import { ProfileValidationSchema, deserializeProfileFormData, serializeProfileFormData } from './lib/profile/profileUtils';
export type { ProfileFormValues } from './lib/profile/profileUtils';

import { ENVIRONMENT_FIELD_DEFAULTS, ENVIRONMENT_FIELD_ORDER, ENVIRONMENT_SECTION } from './lib/environment/environment.constants';
import { buildFieldDefinition, parseFieldValue, validateFieldValue } from './lib/environment/environmentField.helpers';
import { EnvironmentFieldRow } from './ui/environment/EnvironmentFieldRow';
export type { EnvironmentFieldDefinition } from './lib/environment/environmentField.helpers';

import { PreferencesFormContent } from './ui/preferences/PreferencesFormContent';

import { AIPersonalityFormContent } from './ui/ai-personality/AIPersonalityFormContent';
import { SettingsFormProvider } from './ui/ai-personality/SettingsFormProvider';
import { MemoryFormContent } from './ui/memory/MemoryFormContent';

import ConfigurationsPanel from './ui/ai-configuration/ConfigurationsPanel';
import ModelCapabilitiesPanel from './ui/ai-configuration/ModelCapabilitiesPanel';
import OpenAITemplate from './ui/ai-configuration/OpenAITemplate';
import ProjectAIConfiguration from './ui/ai-configuration/ProjectAIConfiguration';
import { RequestModelConnection } from './ui/ai-configuration/RequestModelConnection';
import { useConfigurationsBySection } from './lib/ai-configuration/useConfigurationsBySection';
import { useModelConfigurationLayer } from './lib/ai-configuration/useModelConfigurationLayer';
import { useModelsQuery } from './api/ai-configuration/api';

/** Users tab (`pages/settings/Users.tsx`). */
export const usersFeature = { useUsersActions, UsersPageContent };

/** System-prompts tab (`pages/settings/ServicePrompts.tsx`). */
export const servicePromptsFeature = { ServicePromptsBody };

/**
 * Secrets tab (`pages/settings/Secrets.tsx`).
 *
 * `SecretValueCell` is also consumed by `pages/admin/AdminSecretsTable.tsx`
 * (unit A14): the GLOBAL vault is a different store with a different API, but
 * the masked-value cell — reveal toggle, and a copy that re-fetches the
 * plaintext instead of reading the rendered text — is the same component.
 */
export const secretsFeature = { SecretsTable, SecretValueCell, useSecretPermissions };

/** Project-context tab (`pages/settings/ProjectContext.tsx`). */
export const projectContextFeature = { ProjectContextBody, ProjectContextEmptyState, ProjectContextToasts, ProjectParamsHeader, hasSavedProjectContext, projectContextStyles };

/**
 * General tab (`pages/settings/ProjectGeneral.tsx`) — the PROJECT section's
 * first tab and the tab `/settings` opens on.
 *
 * `ProjectParamsHeader` is NOT re-exported here: it lives in the
 * project-context bundle because that is where the file sits, and General
 * reaches it through `projectContextFeature`. One export per component.
 */
export const projectGeneralFeature = { AgentPipelineBuilder, ProjectAIConfigurationSection };

/**
 * Profile + Personalization tabs (`pages/settings/Profile.tsx`,
 * `pages/settings/Personalization.tsx`).
 */
export const profileFeature = { useDefaultModel, ProfileFormContent, ProfileIdentity, ProfileValidationSchema, deserializeProfileFormData, serializeProfileFormData, formatLastLogin, selectLastLogin };

/** Preferences tab (`pages/settings/Preferences.tsx`). */
export const preferencesFeature = { PreferencesFormContent };

/**
 * AI-Personality tab (`pages/settings/AIPersonality.tsx`).
 *
 * `SettingsFormProvider` is the save mechanism Settings › Memory shares —
 * one fetch, one Formik host, one `PUT /social/author` — which is why it is
 * exported from this bundle rather than duplicated in `memoryFeature`.
 */
export const aiPersonalityFeature = { AIPersonalityFormContent, SettingsFormProvider };

/** Memory tab (`pages/settings/Memory.tsx`) — hosted by `aiPersonalityFeature.SettingsFormProvider`. */
export const memoryFeature = { MemoryFormContent };

/** Environment tab (`pages/settings/Environment.tsx`). */
export const environmentFeature = { ENVIRONMENT_FIELD_DEFAULTS, ENVIRONMENT_FIELD_ORDER, ENVIRONMENT_SECTION, buildFieldDefinition, parseFieldValue, validateFieldValue, EnvironmentFieldRow };

/**
 * AI-configuration tab (`pages/settings/AIConfiguration.tsx`).
 *
 * `ModelCapabilitiesPanel` + `useModelConfigurationLayer` are the baseline's
 * `Configuration/ModelConfiguration.jsx` level, which the port had skipped
 * (issue #80): the chips component existed with no importer, and the helpers
 * it needs had no caller. The panel carries the model picker WITH the chips,
 * so the page cannot mount one without the other.
 *
 * `RequestModelConnection` is the user-facing half of the moderation flow: a
 * member who cannot create a configuration asks an operator for a provider or
 * a model, over the same `centry.moderation_state` wire the App Catalogue's
 * "Request Access" card uses. It joins this bundle (rather than taking a slot
 * of its own) because the page it mounts on is this one.
 */
export const aiConfigurationFeature = {
  ConfigurationsPanel,
  ModelCapabilitiesPanel,
  OpenAITemplate,
  ProjectAIConfiguration,
  RequestModelConnection,
  useConfigurationsBySection,
  useModelConfigurationLayer,
  useModelsQuery,
};
