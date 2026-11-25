/**
 * The console item ids a page passes to the read-only wrapper (ADR-0059
 * decision 6; R3.4b, issue 2613): the server's ids
 * (`internal/adminaccess/console.go`), so the level a page reads is the one
 * the capability document gives that item. A Configuration tab that stands
 * for its own item (R3.5a, issue 2535; R3.6, issue 2537) reads that item.
 */
/**
 * The permissions of actions narrower than their page's item, for
 * `useAdminActionControl`. The names are the server's (`internal/adminaccess`).
 */
export const ADMIN_ACTION_PERMISSION = {
  usersManage: 'admin.auth.users',
  roleCreate: 'configuration.roles.roles.create',
  roleRename: 'configuration.roles.roles.edit',
  roleDelete: 'configuration.roles.roles.delete',
  rolePermissionsEdit: 'configuration.roles.permissions.edit',
  projectCreate: 'projects.projects.project.create',
  projectDelete: 'projects.projects.project.delete',
  projectsEdit: 'projects.projects.projects.edit',
  toolkitTypeManage: 'toolkit_catalogue.type.manage',
  mcpCatalogueManage: 'mcp.catalogue.manage',
  retentionManage: 'retention.manage',
  legalHoldManage: 'legal_hold.manage',
  operationsManage: 'platform.operations.manage',
  descriptorActivate: 'provider_hub.descriptor.activate',
  nativeClients: 'configuration.native_clients',
  microsoftApps: 'configuration.microsoft_apps',
} as const;

export const ADMIN_ITEM = {
  users: 'users',
  access: 'access',
  identityProviders: 'identity-providers',
  /** Not a page: the trust half of Identity providers and SCIM (R3.6). */
  identityTrust: 'identity-trust',
  scim: 'scim',
  projects: 'projects',
  appRequests: 'app-requests',
  providersModels: 'providers-models',
  modelsPricing: 'models-pricing',
  toolPrices: 'tool-prices',
  llmGovernance: 'llm-governance',
  budgetsAlerts: 'budgets-alerts',
  connectorRequests: 'connector-requests',
  delegationReview: 'delegation-review',
  toolkits: 'toolkits',
  credentialInventory: 'credential-inventory',
  permissionTrimming: 'permission-trimming',
  killSwitch: 'kill-switch',
  dataRetention: 'data-retention',
  configuration: 'configuration',
  features: 'features',
  branding: 'branding',
  email: 'email',
  schedulesTasks: 'schedules-tasks',
  serviceDescriptors: 'service-descriptors',
  secrets: 'secrets',
  clientApplications: 'client-applications',
} as const;
