/**
 * Permission-string constants ported from
 * apps/elitea-ui/src/common/constants.js:521-616 (unit S3, spec §9.3).
 *
 * These are the RBAC permission-string identifiers the backend checks
 * (`configuration.*`, `models.*`) — opaque strings, not brand/design tokens
 * or route definitions. P8 in the spec notes the old app's route guards
 * never actually supplied these; unit R1/R2 are what wires real permission
 * checks — this file is just the typed string catalogue.
 */

export const PERMISSIONS = {
  chat: {
    list: 'models.chat.conversations.list',
    create: 'models.chat.conversations.create',
    canvas: {
      create: 'models.chat.canvas.create',
      update: 'models.chat.canvas.update',
    },
    folders: {
      get: 'models.chat.folders.get',
      create: 'models.chat.folders.create',
      update: 'models.chat.folders.update',
      delete: 'models.chat.folders.delete',
    },
  },
  applications: {
    list: 'models.applications.public_applications.list',
    /**
     * The PROJECT-scoped agent list, as opposed to `list`'s public feed.
     *
     * Both strings exist in the legacy RBAC catalogue (`testdata/legacy/
     * legacy-rbac-static-catalog.json`), but only this one is granted by the
     * RBAC seed a fresh Go deployment ships with. See `PERMISSION_GROUPS`.
     */
    projectList: 'models.applications.applications.list',
    create: 'models.applications.applications.create',
    publish: 'models.applications.publish.post',
    export: 'models.applications.export_import.export',
    fork: 'models.applications.fork.post',
    delete: 'models.applications.application.delete',
    update: 'models.applications.application.update',
  },
  pipelines: {
    list: 'models.applications.public_applications.list',
    create: 'models.applications.applications.create',
    publish: 'models.applications.publish.post',
    export: 'models.applications.export_import.export',
    fork: 'models.applications.fork.post',
    delete: 'models.applications.application.delete',
  },
  /**
   * `skills.publish` is the string
   * `internal/api/router.go` mounts the four project-scoped skill-publishing
   * routes behind. The other skill verbs the legacy catalogue carries
   * (`list`/`create`/`update`/`delete`) are not declared here because nothing
   * in this app gates on them yet, and a constant with no reader is what the
   * dead-code gate exists to catch.
   */
  skills: {
    publish: 'models.applications.skills.publish',
  },
  users: {
    view: 'configuration.users.users.view',
    edit: 'configuration.users.users.edit',
    create: 'configuration.users.users.create',
    delete: 'configuration.users.users.delete',
  },
  projectContext: {
    view: 'models.project_context.view',
    edit: 'models.project_context.edit',
  },
  secrets: {
    view: 'configuration.secrets.secret.view',
    list: 'configuration.secrets.secret.list',
    edit: 'configuration.secrets.secret.edit',
    create: 'configuration.secrets.secret.create',
    delete: 'configuration.secrets.secret.delete',
    hide: 'configuration.secrets.secret.hide',
    unsecret: 'configuration.secrets.secret.unsecret',
  },
  artifacts: {
    create: 'configuration.artifacts.artifacts.create',
    delete: 'configuration.artifacts.artifacts.delete',
    view: 'configuration.artifacts.artifacts.view',
    buckets: {
      delete: 'configuration.artifacts.buckets.delete',
      update: 'configuration.artifacts.buckets.update',
      create: 'configuration.artifacts.buckets.create',
      view: 'configuration.artifacts.buckets.view',
    },
  },
  toolkits: {
    list: 'models.applications.tools.list',
    details: 'models.applications.tool.details',
    create: 'models.applications.tools.create',
    update: 'models.applications.tool.update',
    delete: 'models.applications.tool.delete',
    patch: 'models.applications.tool.patch',
    fork: 'models.applications.fork.post',
    export: 'models.applications.tools.export',
  },
  configuration: {
    delete: 'configurations.configuration.delete',
    update: 'configurations.configuration.update',
  },
  litellm: {
    section: 'configuration.litellm',
    edit: 'configuration.litellm.edit',
  },
  index: {
    schedule: 'models.applications.index_meta.edit',
  },
  /**
   * Agent Evaluation — the dimension library, the datasets and the runs.
   *
   * The baseline's `EVAL_PERMISSIONS` block carries seventeen strings across
   * dimensions, suites, datasets, runs and human scores. TEN are declared here,
   * and the other seven are not: no route in this deployment gates on
   * `suite.*`, `human_score.*` or `run.delete`, nothing grants them, and a
   * constant with no reader is what the dead-code gate exists to catch. They
   * arrive with the routes.
   *
   * There is deliberately no `runCancel`. The reference declares none, and the
   * cancel route is gated on `run.create` — the right that started a run is the
   * right that stops it.
   *
   * Gated in `internal/api/router.go`, granted by
   * `migrations/shared/0104_evaluation_dimension_permissions.sql` and
   * `migrations/shared/0116_evaluation_dataset_run_permissions.sql` — reads to
   * admin/editor/viewer, writes to admin/editor.
   */
  evaluation: {
    dimensionRead: 'models.applications.evaluation.dimension.read',
    dimensionCreate: 'models.applications.evaluation.dimension.create',
    dimensionUpdate: 'models.applications.evaluation.dimension.update',
    dimensionDelete: 'models.applications.evaluation.dimension.delete',
    datasetRead: 'models.applications.evaluation.dataset.read',
    datasetCreate: 'models.applications.evaluation.dataset.create',
    datasetUpdate: 'models.applications.evaluation.dataset.update',
    datasetDelete: 'models.applications.evaluation.dataset.delete',
    runRead: 'models.applications.evaluation.run.read',
    runCreate: 'models.applications.evaluation.run.create',
  },
} as const;

/**
 * `constants.js:609-616` — the permission a nav entity needs at minimum to
 * render.
 *
 * `agents`/`pipelines` accept EITHER the public-feed list permission the
 * baseline names or the project-scoped one (`applications.projectList`).
 * The baseline's single string is `models.applications.public_applications.
 * list`, which the Go RBAC seed does not grant to any role — so on a real
 * install both rows vanished from the rail while Chats, Skills, Toolkits,
 * MCPs, Credentials, Applications and Artifacts all rendered, and the
 * production reference shows all three of Chats/Agents/Pipelines. A caller
 * who can list the project's own agents can use both screens; gating them on
 * a permission nothing issues only ever hid working pages.
 */
export const PERMISSION_GROUPS = {
  chat: [PERMISSIONS.chat.folders.get],
  agents: [PERMISSIONS.applications.list, PERMISSIONS.applications.projectList],
  pipelines: [PERMISSIONS.pipelines.list, PERMISSIONS.applications.projectList],
  credentials: [PERMISSIONS.toolkits.list],
  artifacts: [PERMISSIONS.artifacts.view],
  toolkits: [PERMISSIONS.toolkits.list],
} as const;
