import { ToolTypes } from './toolForm';
import { ToolOptionsByType } from './toolOptions';

/**
 * `ToolInitialValues` in the baseline's `consts.js` (Wave-2 promotion pass,
 * Part 2) — default `{ type, settings }` seed per tool type, keyed by
 * `ToolTypes[key].value`. Split into its own file (rather than living in
 * `toolForm.ts`) purely to stay under the §3.5 400-line-per-file budget.
 *
 * The OpenAPI seed uses the current `spec` and configuration-reference
 * contract. The edit path still normalizes legacy `schema_settings` records.
 */

interface ToolInitialValue {
  readonly type: string;
  readonly name?: string;
  readonly description?: string;
  readonly settings: Readonly<Record<string, unknown>>;
}

function selectedTools(type: string): readonly string[] {
  return (ToolOptionsByType[type] ?? []).map((option) => option.value);
}

/**
 * Every `selected_tools` default below is derived from `ToolOptionsByType`
 * (`./toolOptions.ts`) rather than copy-pasted a second time, but the
 * resulting VALUES are identical to the baseline's hard-coded arrays.
 */
export const ToolInitialValues: Readonly<Record<string, ToolInitialValue>> = {
  [ToolTypes.artifact.value]: {
    type: ToolTypes.artifact.value,
    settings: { bucket: '', selected_tools: selectedTools('artifact') },
  },
  [ToolTypes.open_api.value]: {
    type: ToolTypes.open_api.value,
    settings: {
      spec: '',
      selected_tools: [],
    },
  },
  [ToolTypes.custom.value]: {
    type: ToolTypes.custom.value,
    name: 'Custom tool',
    description: 'custom tool',
    settings: {},
  },
  [ToolTypes.confluence.value]: {
    type: ToolTypes.confluence.value,
    settings: {
      base_url: '',
      cloud: true,
      limit: 5,
      max_pages: 10,
      number_of_retries: 2,
      min_retry_seconds: 10,
      max_retry_seconds: 60,
      space: '',
      selected_tools: selectedTools('confluence'),
    },
  },
  [ToolTypes.github.value]: {
    type: ToolTypes.github.value,
    settings: {
      base_url: 'https://api.github.com',
      repository: '',
      active_branch: 'main',
      app_id: null,
      app_private_key: null,
      access_token: null,
      username: null,
      password: null,
      selected_tools: selectedTools('github'),
    },
  },
  [ToolTypes.gitlab.value]: {
    type: ToolTypes.gitlab.value,
    settings: { url: '', repository: '', branch: 'main', private_token: '', selected_tools: selectedTools('gitlab') },
  },
  [ToolTypes.gitlab_org.value]: {
    type: ToolTypes.gitlab_org.value,
    settings: { url: '', repositories: '', branch: 'main', private_token: '', selected_tools: selectedTools('gitlab') },
  },
  [ToolTypes.bitbucket.value]: {
    type: ToolTypes.bitbucket.value,
    settings: {
      url: '',
      project: '',
      repository: '',
      branch: 'main',
      username: '',
      password: '',
      selected_tools: selectedTools('bitbucket'),
    },
  },
  [ToolTypes.jira.value]: {
    type: ToolTypes.jira.value,
    settings: {
      base_url: '',
      token: null,
      api_key: null,
      username: null,
      cloud: true,
      limit: 5,
      additional_fields: '',
      verify_ssl: true,
      selected_tools: selectedTools('jira'),
    },
  },
  [ToolTypes.yagmail.value]: {
    type: ToolTypes.yagmail.value,
    settings: { host: null, username: null, password: null, selected_tools: selectedTools('yagmail') },
  },
  [ToolTypes.report_portal.value]: {
    type: ToolTypes.report_portal.value,
    settings: { endpoint: '', api_key: '', project: '', selected_tools: selectedTools('report_portal') },
  },
  [ToolTypes.application.value]: {
    type: ToolTypes.application.value,
    settings: { application_id: '', application_version_id: '', variables: [] },
  },
  [ToolTypes.testrail.value]: {
    type: ToolTypes.testrail.value,
    settings: { url: null, email: null, password: null, selected_tools: selectedTools('testrail') },
  },
  [ToolTypes.ado_boards.value]: {
    type: ToolTypes.ado_boards.value,
    settings: { organization_url: null, project: null, token: null, limit: 5, selected_tools: selectedTools('ado_boards') },
  },
  [ToolTypes.ado_wiki.value]: {
    type: ToolTypes.ado_wiki.value,
    settings: { organization_url: null, project: null, token: null, selected_tools: selectedTools('ado_wiki') },
  },
  [ToolTypes.ado_plans.value]: {
    type: ToolTypes.ado_plans.value,
    settings: { organization_url: null, project: null, token: null, selected_tools: selectedTools('ado_plans') },
  },
  [ToolTypes.ado_repos.value]: {
    type: ToolTypes.ado_repos.value,
    settings: {
      organization_url: null,
      repository_id: null,
      project: null,
      token: null,
      base_branch: 'main',
      active_branch: 'main',
      selected_tools: selectedTools('ado_repos'),
    },
  },
  [ToolTypes.testio.value]: {
    type: ToolTypes.testio.value,
    settings: { endpoint: '', api_key: '', selected_tools: selectedTools('testio') },
  },
  [ToolTypes.xray_cloud.value]: {
    type: ToolTypes.xray_cloud.value,
    settings: { base_url: '', client_id: '', client_secret: '', limit: '100', selected_tools: selectedTools('xray_cloud') },
  },
  [ToolTypes.zephyr.value]: {
    type: ToolTypes.zephyr.value,
    settings: { base_url: '', username: null, password: null, selected_tools: selectedTools('zephyr') },
  },
  [ToolTypes.zephyr_scale.value]: {
    type: ToolTypes.zephyr_scale.value,
    settings: {
      base_url: '',
      token: null,
      username: null,
      password: null,
      cookies: null,
      selected_tools: selectedTools('zephyr_scale'),
    },
  },
  [ToolTypes.qtest.value]: {
    type: ToolTypes.qtest.value,
    settings: { base_url: '', project_id: null, qtest_api_token: null, selected_tools: selectedTools('qtest') },
  },
  [ToolTypes.google.value]: {
    type: ToolTypes.google.value,
    settings: { token_json: '', selected_tools: selectedTools('google') },
  },
  [ToolTypes.sharepoint.value]: {
    type: ToolTypes.sharepoint.value,
    settings: { url: '', client_id: '', client_secret: '', selected_tools: selectedTools('sharepoint') },
  },
  [ToolTypes.rally.value]: {
    type: ToolTypes.rally.value,
    settings: {
      server: '',
      api_key: '',
      username: '',
      password: '',
      workspace: '',
      project: '',
      selected_tools: selectedTools('rally'),
    },
  },
  [ToolTypes.sql.value]: {
    type: ToolTypes.sql.value,
    settings: {
      dialect: '',
      host: '',
      port: '',
      username: '',
      password: '',
      database_name: '',
      selected_tools: selectedTools('sql'),
    },
  },
  [ToolTypes.sonar.value]: {
    type: ToolTypes.sonar.value,
    settings: { url: '', sonar_token: '', sonar_project_name: '', selected_tools: selectedTools('sonar') },
  },
  [ToolTypes.google_places.value]: {
    type: ToolTypes.google_places.value,
    settings: { api_key: '', results_count: '', selected_tools: selectedTools('google_places') },
  },
};
