/**
 * GENERATED FILE: do not edit. Regenerate with `npm run contract:gen`.
 *
 * Types derived from the pinned server contract (contract/elitea-v2.yaml,
 * Apache-2.0, see contract/LICENSE.elitea) merged with the Agent Zefir
 * native-gap overlay and pruned to contract/client-operations.json.
 *
 * upstream sha256:    56d6638fdcad882675464d031c2d29e1c19f0fe69e70dbf75f3cebce3658b551
 * overlay sha256:     f00053bae6cdc5d0a0be49be5198a449ee7f17b599d5af2069c6258b82421bc9
 * operations sha256:  989fa26b8e9e045e1840b590859d6760a8770e584456d0fc41ff97484ea961ae
 */
export interface paths {
    "/.well-known/elitea-client": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** Anonymous discovery document a client reads before signing in */
        get: operations["getClientDiscovery"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/auth/native/revoke": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Revoke the device session a native token belongs to (RFC 7009)
         * @description Revokes the whole device session. Unknown or already revoked tokens also answer 200, so the answer reveals nothing. Never refused for an old client version.
         */
        post: operations["revokeNativeToken"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/auth/native/token": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Exchange an authorization code or a refresh token (native public client)
         * @description Form-encoded, no client authentication (an Authorization header is refused). The refresh token rotates on every use; re-presenting the immediately previous one within the server's re-delivery window (default 30 s) returns the same successor pair, any other reuse revokes the device session. Below the client's minimum version the answer is 426 before anything is consumed.
         */
        post: operations["exchangeNativeToken"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/branding/pack.json": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /** The deployment's resolved brand pack, with absolute asset URLs */
        get: operations["getBrandingPackJSON"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/configurations/models/{project_id}": {
        parameters: {
            query?: {
                include_shared?: boolean;
                section?: string;
            };
            header?: never;
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        /**
         * List the models a project can select
         * @description NOTE(W2): internal/api/v2/configurations/handler.go, ListModels
         *     (compatibility) and internal/application/configurations/models.go
         *     (reviewed). The compatibility handler reads the five model rows of the
         *     tenant `configuration` table: llm_model, embedding_model, asr_model,
         *     tts_model and image_generation_model. It reads at most 1000 rows.
         *
         *     THE ROWS CARRY NO SERVER-ASSIGNED `id` ON THE REVIEWED ROUTE. The
         *     shipped client synthesises one from `project_id` and `name`
         *     (apps/elitea-web/src/features/credentials/api/configurationConnections.ts,
         *     withModelId). Do not read `id` as an identifier the server assigned.
         *
         *     The app calls this through a hand-written client today, and four
         *     separate slices re-derived that client independently. It is described
         *     here so one generated client replaces all four.
         */
        get: operations["listConfigurationModels"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/application/prompt_lib/{project_id}/{application_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                application_id: number;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        /**
         * Retrieve full application details
         * @description NOTE(W2): internal/api/v2/applications/handler.go:110-139.
         */
        get: operations["getApplication"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/applications/prompt_lib/{project_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        /**
         * List applications for a project
         * @description NOTE(W2): internal/api/v2/applications/handler.go:83-119 — the
         *     response is the domain ListResponse (types.go:105-111). The handler
         *     also reads query/search, tags, folder_id, agents_type.
         */
        get: operations["listApplications"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/conversation/prompt_lib/{project_id}/{conversation_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The conversation's numeric id or its uuid; the server accepts either. */
                conversation_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        /** Read one conversation, optionally with its latest message groups */
        get: operations["getConversation"];
        put?: never;
        post?: never;
        /** Delete a conversation and its messages */
        delete: operations["deleteConversation"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/conversations/prompt_lib/{project_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        /**
         * List conversations, optionally scoped to one participant entity
         * @description Backs the run-history panel (issue #868): entity_name + entity_meta_id
         *     narrow the listing to conversations whose `meta.single_participant`
         *     is that one agent/pipeline (`entity_name=application`) or toolkit/MCP
         *     (`entity_name=toolkit`) — internal/api/v2/conversations/handler.go's
         *     List, registered at router.go's models.chat.conversations.list route.
         *
         *     Hidden (support/DeepWiki) conversations are excluded unless
         *     hidden=only. mine=true narrows to the CALLER's own conversations;
         *     with no resolvable caller it answers an empty page rather than
         *     everybody's — see the handler's own comment on why that is a safety
         *     property, not a convenience.
         *
         *     The route also serves POST (create a conversation), which is
         *     handwritten in entities/conversation's own conversationApi and is not
         *     documented here.
         */
        get: operations["listConversations"];
        put?: never;
        /** Create a conversation in a project */
        post: operations["createConversation"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/messages/prompt_lib/{project_id}/{conversation_uuid}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The conversation's uuid (a send takes the uuid only; the list also takes the numeric id). */
                conversation_uuid: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        /**
         * A conversation's transcript, paged or as a delta
         * @description Legacy mode pages message groups newest first (`limit`, `offset`).
         *     With `changes_since` it answers the groups changed since the cursor,
         *     oldest change first, the tombstones of deleted groups and the next
         *     cursor. A group still streaming changes again when it settles, so a
         *     delta can return it twice; upsert by `id`. The cursor is bound to the
         *     conversation's numeric id, whichever spelling the path used.
         */
        get: operations["listConversationMessages"];
        put?: never;
        /**
         * Send a user message to the conversation's agent or model and start its execution
         * @description Idempotent per `question_id`: re-sending the byte-identical body
         *     returns the original execution with `created: false`; a different body
         *     with the same `question_id` is a 409. The response's `events_url` is a
         *     same-origin absolute path to the execution's event stream.
         *
         *     `agent.execute.application.v1` addresses the conversation's agent
         *     (`application` participant). `agent.execute.adhoc.v1` is a plain model
         *     turn: `participant_id` is the conversation's `dummy` participant, or
         *     0 when it has none, and `llm_settings.model_name` is required (the
         *     server does not read the model from the participant: without
         *     `llm_settings` it answers 422 `unsupported_agent_execution`).
         */
        post: operations["startApplicationTurn"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/notifications/notifications/prompt_lib/{project_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        /**
         * List the caller's notifications, paged or as a delta
         * @description With `changes_since` it answers the caller's notifications changed
         *     since the cursor, oldest change first, plus tombstones. Combining it
         *     with `offset`, `only_new`, `only_total`, `search`, `event_type`,
         *     `sort_by` or `sort_order` is a 400 `invalid_sync_request`.
         */
        get: operations["listNotifications"];
        /** Mark notifications seen or unseen */
        put: operations["updateNotifications"];
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/projects/project/default/{public_project_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                public_project_id: number;
            };
            cookie?: never;
        };
        /**
         * List projects accessible to the caller
         * @description NOTE(W2): PLAIN ARRAY of ProjectWithGroups. The route is
         *     internal/api/v2/projects/handler.go:157 (GetCurrentProjectList), which
         *     calls getCurrentProjects at :161. The Project struct is at :134-143.
         *     assembleProjects at :227-252 collapses the joined rows into one item
         *     per project.
         *
         *     The handler reads `check_public_role` as presence only. Any value, also
         *     `false`, sets the flag true. The flag reaches the query, which then
         *     hides the public project from a caller who is not an admin of it.
         */
        get: operations["listProjects"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
}
export type webhooks = Record<string, never>;
export interface components {
    schemas: {
        /** @description NOTE(W2): internal/domain/applications/types.go:15-50 — required set = json tags WITHOUT omitempty (id, name, tags, created_at, owner_id, is_forked, meta, has_interrupt) PLUS updated_at, whose omitempty is ineffective on a time.Time (always marshaled; zero sentinel when unscanned). Every other omitempty field is optional (absent when zero-valued). */
        Application: {
            agent_type?: string;
            authors?: components["schemas"]["Author"][];
            /** Format: date-time */
            created_at: string;
            created_by?: string;
            description?: string;
            folder_id?: string;
            has_interrupt: boolean;
            icon?: string;
            id: string;
            is_forked: boolean;
            meta: {
                [key: string]: unknown;
            } | null;
            metadata?: {
                [key: string]: unknown;
            };
            name: string;
            owner_id: string;
            project_id?: string;
            status?: string;
            /** @description Tag NAMES, deduplicated and sorted, taken from every version of the application (repos/applications.go List, tagsExpr). ALWAYS present: an application with no tags carries an empty array, not null and not an absent key (issue 841). Only the LIST response fills it; Get and Create build their own maps. */
            tags: string[];
            type?: string;
            /**
             * Format: date-time
             * @description When the agent last changed. ALWAYS present despite the omitempty tag — encoding/json never omits struct types. It used to be the zero sentinel "0001-01-01T00:00:00Z" on every row, because `applications` had no such column and nothing scanned one: migrations/tenant/0134 adds it and every write stamps it inside the statement that makes the change — a rename, a version save, a new version, a version delete and a default-version change (repos/applications.go). A request that changes nothing does not move it.
             */
            updated_at: string;
        };
        /** @description NOTE(W2): Get response map, internal/api/v2/applications/handler.go:121-139. version_details is present only when the application has at least one version whose detail row loads (:132-137). */
        ApplicationDetail: {
            /** Format: date-time */
            created_at: string;
            description: string;
            icon: string;
            id: string;
            name: string;
            /** @description Populated from applications.created_by. */
            owner_id: string;
            version_details?: components["schemas"]["ApplicationVersionDetail"];
            versions: components["schemas"]["ApplicationVersionSummary"][];
        };
        /** @description NOTE(W2): internal/domain/applications/types.go:86-92 (ListResponse), marshaled directly by List (internal/api/v2/applications/handler.go:101-107). */
        ApplicationList: {
            page: number;
            page_size: number;
            rows: components["schemas"]["Application"][];
            total: number;
            total_pages: number;
        };
        /**
         * @description `llm_settings` must be absent for `agent.execute.application.v1` and
         *     present for `agent.execute.adhoc.v1`; `attachments_info` absent or
         *     empty. The whole body must stay at or under 512 KiB.
         */
        ApplicationTurnRequest: {
            /** @description Must equal the path's conversation uuid. */
            conversation_uuid: string;
            /** @description The model of an adhoc turn; absent on an application turn. */
            llm_settings?: {
                model_name: string;
                /** @description The project the model belongs to, when it is shared from another project. */
                model_project_id?: number;
                stream?: boolean;
            } & {
                [key: string]: unknown;
            };
            /**
             * @description The agent participant's row id (application contract, at least 1),
             *     or the `dummy` participant's row id or 0 (adhoc contract).
             */
            participant_id: number;
            payload: {
                user_input: string;
            };
            /** @description Must equal the path's project id. */
            project_id: number;
            /** @description Client-generated LOWERCASE canonical UUID; the idempotency key of the turn. */
            question_id: string;
        };
        ApplicationTurnStarted: {
            command_id: string;
            /** @description False when an identical earlier send was replayed. */
            created: boolean;
            /** @description Same-origin absolute path, `/api/v2/executions/{project_id}/{execution_id}/events`. */
            events_url: string;
            execution_id: string;
            response_message_id: string;
            task_id: string;
        };
        /** @description NOTE(W2): union of the version-detail maps the handlers emit. Field presence varies by endpoint (hence the minimal required set): fetchVersionDetails always adds created_at/agent_type/instructions/ welcome_message/llm_settings/meta/conversation_starters/ pipeline_settings/author_id/author/tools/tags/variables (internal/api/v2/applications/handler.go). The application and version create paths return that same stored pipeline-settings and tag state. The author object contains id, email, and name from auth_core__user. UpdateVersion emits a smaller subset without created_at, author, or tools. Fork adds is_forked and omits pipeline_settings. The import detail map has no is_forked and omits created_at, but carries meta, variables, and tags from the canonical stores. Public application details omit variables, created_at, author, and is_forked. Empty-vs-missing: instructions and welcome_message are always present as "" when the DB column is NULL (COALESCE / pointer deref to "" in applications/handler.go). */
        ApplicationVersionDetail: {
            agent_type?: string;
            application_id: string;
            author?: components["schemas"]["Author"];
            author_id?: string;
            conversation_starters?: components["schemas"]["ConversationStarters"];
            /** Format: date-time */
            created_at?: string;
            /** @description Numeric id serialized as string. */
            id: string;
            instructions?: string;
            is_forked?: boolean;
            llm_settings?: components["schemas"]["LlmSettings"];
            meta?: components["schemas"]["VersionMeta"] | null;
            name: string;
            /** @description NOTE(#898): `meta.notes`, surfaced beside `meta` as pylon's `hydrate_notes_from_meta` surfaces it. Always a string on the version-detail read and on the two write echoes ("" when the version carries no notes); absent from the other version maps (import, fork, the public catalogue detail), which do not build their response through these two helpers. */
            notes?: string;
            pipeline_settings?: components["schemas"]["PipelineSettings"];
            status: string;
            tags?: components["schemas"]["VersionTag"][];
            tools?: components["schemas"]["VersionToolRef"][];
            variables?: components["schemas"]["VersionVariable"][];
            welcome_message?: string;
        };
        /** @description NOTE(W2): getVersions row map, internal/api/v2/applications/handler.go:216-243. The handler emits ALL eight keys on every row; `required` lists only the five the schema carried before issue 844, so that adding the other three stays additive for the generated fixtures rather than invalidating every existing one. */
        ApplicationVersionSummary: {
            /** @description Kept although pylon's ApplicationVersionListModel excluded it: apps/elitea-web reads it off the summary to shape the version dropdown (pages/agents/lib/editApplicationMappers.ts toVersionSummaries, and its pipeline twin). */
            agent_type: string;
            /** Format: date-time */
            created_at: string;
            /** @description Numeric id serialized as string (strconv.Itoa). */
            id: string;
            /** @description The version's prompt text, always present as "" when the column is NULL (COALESCE in getVersions). Restored for issue 844 — the projection dropped it, so a client reading the prompt off the version LIST saw null on a response that otherwise looked complete. */
            instructions?: string;
            /** @description Derived, not stored: true when the owning application's meta.default_version_id names this row. Emitted since the default-version bar was wired, and read by apps/elitea-web/src/pages/{agents,pipelines}/lib/*Mappers.ts and features/agents/model/useVersionBarCommands.ts — it was simply absent from this schema, which is why those readers cast it off the wire. */
            is_default?: boolean;
            /** @description application_versions.meta, the same jsonb ApplicationVersionDetail carries. Restored for issue 844 alongside instructions; `{}` when the column is NULL. */
            meta?: components["schemas"]["VersionMeta"] | null;
            name: string;
            status: string;
        };
        /** @description NOTE(W2): internal/domain/applications/types.go:5-9 (no omitempty on any field) and the inline author maps in internal/api/v2/applications/handler.go:451, 600, 790. */
        Author: {
            email: string;
            id: string;
            name: string;
        };
        /** @description A brand pack conforming to https://elitea.ai/schemas/brand-pack/1.json. NOTE(W2): mirrors internal/api/v2/branding/pack.go:123 field for field (the Go mirror of apps/elitea-web/src/shared/brand/schema.ts). */
        BrandPack: {
            $schema: string;
            assets: {
                favicon: string;
                loginArt?: string;
                /** @description Raster logo for e-mail, a same-origin asset path. */
                logoEmail?: string;
                logoFull: string;
                logoMark: string;
            };
            brand: {
                hue: string;
                onBrand?: string;
            };
            id: string;
            locale: {
                dateLocale: string;
                default: string;
            };
            product: {
                docsUrl?: string;
                name: string;
                /** @description Display name outbound e-mail is sent as. */
                senderName?: string;
                shortName: string;
                /** @description Address the app's contact-support paths use. */
                supportEmail?: string;
                supportUrl?: string;
                tagline?: string;
            };
            schemes: {
                dark: {
                    [key: string]: string;
                };
                hc?: {
                    [key: string]: string;
                };
                light: {
                    [key: string]: string;
                };
            };
            shape: {
                /** @enum {string} */
                density: "comfortable" | "compact";
                radiusLg: number;
                radiusMd: number;
                radiusPill: number;
                radiusSm: number;
            };
            typography: {
                baseSize: number;
                /** @description Self-hosted faces the web app declares as @font-face. */
                fontFaces?: {
                    family: string;
                    /** @enum {string} */
                    style?: "normal" | "italic";
                    /** @description Same-origin path of an uploaded font asset. */
                    url: string;
                    weight?: string;
                }[];
                fontFamily: string;
                fontFamilyMono: string;
                scale: number;
            };
            version: string;
        };
        /** @description The part of the native client policy published before sign-in. */
        ClientPublicPolicy: {
            /** @description Deployment-wide minimum client version; empty for none. */
            min_client_version: string;
            offline_enabled: boolean;
            require_device_lock: boolean;
        };
        ClientUpgradeRequiredError: {
            /** @enum {string} */
            error: "client_upgrade_required";
            error_description?: string;
            min_client_version: string;
        };
        /** @description NOTE(W2): the union of internal/api/v2/configurations/handler.go's Model (compat) and internal/application/configurations/models.go's CurrentModelCatalogItem (reviewed). */
        ConfigurationModelItem: {
            /** @description `compat` only. A copy of `id`. */
            config_id?: number;
            /** @description `compat` only. A copy of `name`. */
            config_name?: string;
            /** @description `reviewed` only. */
            context_window?: number | null;
            data?: {
                [key: string]: unknown;
            };
            /** @description `reviewed` only. The project default. */
            default?: boolean;
            /** @description `reviewed` only. */
            display_name?: string | null;
            /** @description `reviewed` only. */
            high_tier?: boolean | null;
            /** @description `compat` only, and it is the CONFIGURATION row id, not a model id. `reviewed` emits no id at all, and the shipped client synthesises one from `project_id` and `name`. */
            id?: number;
            /** @description `compat` only, and it is ALWAYS false: that handler does not read the project default. Read `default_model_name` on the envelope instead. */
            is_default?: boolean;
            /** @description `reviewed` only. */
            low_tier?: boolean | null;
            /** @description Optional provider input-only ceiling, inside the combined context window. */
            max_input_tokens?: number | null;
            /** @description `reviewed` only. */
            max_output_tokens?: number | null;
            /** @description BOTH. The model name a caller selects. */
            name: string;
            /** @description `reviewed` only. */
            openai_compatible?: boolean | null;
            /** @description BOTH, and a NUMBER on both, for the reason ConfigurationRow gives. */
            project_id: number;
            /** @description `compat` only. */
            section?: string;
            /** @description `reviewed` only. */
            shared?: boolean;
            /** @description `reviewed` only. */
            supports_reasoning?: boolean | null;
            /** @description `reviewed` only. */
            supports_vision?: boolean | null;
            /** @description `compat` only. One of llm_model, embedding_model, asr_model, tts_model, image_generation_model. */
            type?: string;
        };
        /** @description NOTE(W2): internal/api/v2/configurations/handler.go, ListModels (compat) and internal/application/configurations/models.go, CurrentModelCatalog (reviewed). */
        ConfigurationModelListResponse: {
            /** @description `reviewed` only. `compat` emits neither this key nor the one below, and its `is_default` is always false, so a client reading a default model from a compat deployment gets nothing rather than a wrong answer. */
            default_model_name?: string | null;
            /** @description `reviewed` only. */
            default_model_project_id?: number | null;
            /** @description `reviewed` only, and omitted when unset. */
            high_tier_default_model_name?: string | null;
            /** @description `reviewed` only, and omitted when unset. */
            high_tier_default_model_project_id?: number | null;
            items: components["schemas"]["ConfigurationModelItem"][];
            /** @description `reviewed` only, and omitted when unset. */
            low_tier_default_model_name?: string | null;
            /** @description `reviewed` only, and omitted when unset. */
            low_tier_default_model_project_id?: number | null;
            total: number;
        };
        ConversationDetail: {
            /** Format: date-time */
            created_at: string;
            created_by?: string;
            description?: string;
            folder_id?: string | null;
            /** @description Numeric id, serialised as a string by this endpoint. */
            id: string;
            instructions?: string;
            is_private?: boolean;
            message_count?: number;
            /** @description Present only when `messages_limit` > 0 was requested. */
            message_groups?: components["schemas"]["MessageGroup"][];
            meta?: {
                [key: string]: unknown;
            };
            name: string;
            participants?: components["schemas"]["ConversationParticipant"][];
            /** @description Numeric id, serialised as a string by this endpoint. */
            project_id: string;
            source?: string;
            /** Format: date-time */
            updated_at: string;
            uuid?: string;
        } & {
            [key: string]: unknown;
        };
        ConversationListing: {
            /** @description Delta mode only. The page was cut short by `limit`; call again at once with `next_cursor`. */
            has_more?: boolean;
            /** @description Delta mode only. Opaque; send it as the next `changes_since`. It never passes the server clock minus a 5 s settle window, so recently changed rows are returned again (upsert by id). */
            next_cursor?: string;
            rows: components["schemas"]["ConversationSummary"][];
            /** @description Delta mode only. Conversations deleted or gone from this list for the caller since the cursor. */
            tombstones?: components["schemas"]["SyncTombstone"][];
            total: number;
        };
        /** @description One message group of a transcript; delta rows use the same shape. */
        ConversationMessage: {
            author_participant_id?: number;
            content: string;
            content_type?: string;
            conversation_id: string;
            /** Format: date-time */
            created_at: string;
            /** @description The message group's numeric id AS A STRING (tombstones carry it as an integer). */
            id: string;
            /** @description Present (true) only while the turn is in flight. */
            is_streaming?: boolean;
            /** @description Attachment items only; omitted for a group without attachments. */
            message_items?: {
                [key: string]: unknown;
            }[];
            metadata?: {
                [key: string]: unknown;
            };
            reply_to_id?: number;
            /** @description `user` or `assistant`. */
            role: string;
            sent_to_id?: number;
            task_id?: string;
            /** @description The group's uuid; for an answer, the `response_message_id` of the send. */
            uid: string;
            /**
             * Format: date-time
             * @description When the group was last rewritten; omitted when never.
             */
            updated_at?: string;
        };
        /** @description Legacy mode: `items`, `total`, `page`, `page_size`, `total_pages`. Delta mode: `items` (changed groups, oldest change first), `total` (groups in the conversation now), `tombstones`, `next_cursor`, `has_more`. */
        ConversationMessageListing: {
            /** @description Delta mode only. See ConversationListing.has_more. */
            has_more?: boolean;
            items: components["schemas"]["ConversationMessage"][];
            /** @description Delta mode only. See ConversationListing.next_cursor. */
            next_cursor?: string;
            page?: number;
            page_size?: number;
            /** @description Delta mode only. Message groups deleted since the cursor (`id` is the group id). */
            tombstones?: components["schemas"]["SyncTombstone"][];
            total: number;
            total_pages?: number;
        };
        ConversationParticipant: {
            entity_meta?: {
                [key: string]: unknown;
            } | null;
            /** @description `user`, `application`, `toolkit`, `llm`, ... (open set). */
            entity_name: string;
            entity_settings?: {
                [key: string]: unknown;
            } | null;
            /** @description The participant row id (what a send's `participant_id` names). */
            id: number;
            meta?: {
                [key: string]: unknown;
            } | null;
        };
        /** @description NOTE(W2): opaque jsonb array round-trip — see items marker. One caveat to the "never inspects" claim: runPublishValidation decodes the column as []string (eliteacore/handler.go:1028-1029), so non-string elements are silently read as an empty list there (spurious "no conversation starters" warning); the store/return round-trip itself is untyped. */
        ConversationStarters: unknown[];
        /** @description One row of listConversations — the run-history panel's (issue #868) light projection of a conversation. `duration` is a placeholder: internal/api/v2/conversations/handler.go's List always answers `-1` for it (no per-conversation duration is computed server-side yet); a caller wanting a displayed duration derives one from created_at/updated_at instead of trusting this field. */
        ConversationSummary: {
            created_at: string;
            /** @description Always -1 today; see the schema description. */
            duration: number;
            id: number;
            message_groups_count: number;
            /** @description Carries `single_participant` (the entity this conversation was filtered by, when entity_name/entity_meta_id were sent) among other conversation-level flags (`is_hidden`, etc). */
            meta?: {
                [key: string]: unknown;
            } | null;
            name: string;
            updated_at: string;
        };
        CreateConversationRequest: {
            is_private?: boolean;
            meta?: {
                [key: string]: unknown;
            };
            name?: string;
            participants?: ({
                entity_meta: {
                    [key: string]: unknown;
                };
                entity_name: string;
                entity_settings?: {
                    [key: string]: unknown;
                };
            } & {
                [key: string]: unknown;
            })[];
        };
        /** @description Flat (error is a string), unlike the nested API error envelope. */
        DeviceRevokedError: {
            /** @enum {string} */
            error: "device_revoked";
            error_description?: string;
        };
        /** @description What a deployment tells a client before sign-in. A key is never renamed or removed within one client_contract major version. */
        DiscoveryDocument: {
            /** @description Absolute URL of the brand pack JSON, content-addressed with `?v=`. */
            brand_pack_url: string;
            /** @description Client contract version, `major.minor`. */
            client_contract: string;
            client_policy: components["schemas"]["ClientPublicPolicy"];
            /** @enum {string} */
            deployment_kind: "saas" | "self_hosted";
            /** @description Product name from the deployment's brand pack. */
            display_name: string;
            /** @description Client id to its effective minimum version; a client without an entry falls back to client_policy.min_client_version. */
            min_client_version: {
                [key: string]: string;
            };
            /** @description Null while the deployment has no registered native client. */
            native_auth: components["schemas"]["NativeAuthEndpoints"] | null;
            /** @description `major.minor`, or `dev` for an unversioned build. */
            server_version: string;
        };
        /** @description NOTE(W2): wire truth from pkg/apierr/apierr.go:47-59 (apierr.Write encodes `{"error": message}`) and the handlers' inline `map[string]any{"error": ...}` writes. No operation documented in this spec emits the pylon `ok` field via THIS schema; the five social/handler.go 500 sites (:268, 301, 321, 341, 419) use the named `SocialActionErrorResponse` schema instead (see there), and five more UNDOCUMENTED sites remain on toolkits/handler.go (:286, 312, 324, 350, 362). */
        ErrorResponse: {
            /** @description Human-readable error message */
            error: string;
        };
        LlmSettings: {
            max_tokens?: number;
            model_name?: string;
            /** @description Stored as string or number depending on writer; ExportImportGet normalizes float64 to a numeric string (internal/api/v2/eliteacore/handler.go:2590-2597). */
            model_project_id?: string | number;
            /** @description One of none, low, medium or high. Mutually exclusive with temperature on the runtime profile: the native worker refuses a profile carrying both (services/elitea-worker-rust/src/agents/ assembly.rs, parse_reasoning_effort), and the web write path encodes the same rule (apps/elitea-web/src/shared/api/ agentLlmSettings.ts, selectEffortAndTemperature). */
            reasoning_effort?: string;
            temperature?: number;
            top_p?: number;
        } & {
            [key: string]: unknown;
        };
        MessageGroup: {
            author_participant_id: number;
            /** @description The text items joined with newlines (convenience projection). */
            content?: string;
            /** @description Server-local timestamp without zone, e.g. `2026-10-04 12:00:00.123456`. */
            created_at: string;
            id: number;
            is_streaming: boolean;
            message_items: components["schemas"]["MessageItem"][];
            meta?: {
                [key: string]: unknown;
            } | null;
            reply_to_id?: number;
            sent_to_id?: number;
            task_id?: string;
            updated_at?: string;
            uuid: string;
        } & {
            [key: string]: unknown;
        };
        MessageItem: {
            id: number;
            /** @description For `text_message`, `{content: string}`. */
            item_details?: {
                [key: string]: unknown;
            } | null;
            /** @description `text_message`, `attachment_message`, `canvas_message`, ... (open set; unknown types are skipped). */
            item_type: string;
            order_index: number;
        } & {
            [key: string]: unknown;
        };
        NativeAuthEndpoints: {
            authorization_endpoint: string;
            code_challenge_methods_supported: string[];
            /** @description The deployment origin; equals the `iss` parameter of an authorization response. */
            issuer: string;
            revocation_endpoint: string;
            token_endpoint: string;
        };
        NativeClientPolicy: {
            allow_screenshots: boolean;
            /**
             * Format: int64
             * @description Seconds in the background before the app locks again; 0 means it never relocks on resume. 0 to 86400.
             */
            idle_lock_seconds: number;
            /** @description Effective minimum for this client; empty for none. */
            min_client_version: string;
            offline_attachments: boolean;
            /** Format: int64 */
            offline_max_mb: number;
            /**
             * Format: int64
             * @description 0 disables offline storage.
             */
            offline_retention_days: number;
            require_device_lock: boolean;
        };
        NativeOAuthError: {
            error: string;
            error_description?: string;
        };
        NativeRevokeRequest: {
            client_id: string;
            token: string;
            token_type_hint?: string;
        };
        NativeTokenRequest: {
            client_id: string;
            /** @description Recorded on the device (refresh grant). */
            client_version?: string;
            code?: string;
            /** @description PKCE verifier */
            code_verifier?: string;
            /** @enum {string} */
            grant_type: "authorization_code" | "refresh_token";
            redirect_uri?: string;
            refresh_token?: string;
        };
        NativeTokenResponse: {
            /** @description Opaque bearer token. */
            access_token: string;
            /** @description Present on every 200 unless the server could not read its policy; the client then keeps the last one. */
            client_policy?: components["schemas"]["NativeClientPolicy"];
            /** @description The device session id. */
            device_id: string;
            /**
             * Format: int64
             * @description Seconds until the access token expires.
             */
            expires_in: number;
            /** @description Opaque; rotated on every use. */
            refresh_token: string;
            /**
             * Format: int64
             * @description Idle seconds before the device must sign in again.
             */
            refresh_token_expires_in: number;
            /** @description Bearer. */
            token_type: string;
        };
        Notification: {
            created_at: string;
            event_type: string;
            id: number;
            is_seen: boolean;
            /** @description Event-specific JSON value. */
            meta?: unknown;
            project_id: number;
            updated_at?: string | null;
            user_id: number;
            uuid: string;
        };
        /** @description With `changes_since`, `total` counts all of the caller's notifications and the delta fields are added. */
        NotificationPage: {
            /** @description Delta mode only. See ConversationListing.has_more. */
            has_more?: boolean;
            /** @description Delta mode only. See ConversationListing.next_cursor. */
            next_cursor?: string;
            rows?: components["schemas"]["Notification"][];
            /** @description Delta mode only. Notifications deleted since the cursor. */
            tombstones?: components["schemas"]["SyncTombstone"][];
            total: number;
        };
        NotificationSeenUpdate: {
            /** @description Notification ids, or the string `all`. */
            ids: number[] | "all";
            is_seen: boolean;
        };
        NotificationSeenUpdated: {
            updated: number;
        };
        PipelineSettings: {
            [key: string]: unknown;
        };
        /** @description NOTE(W2): internal/api/v2/projects/handler.go — the Group struct. */
        ProjectGroup: {
            id: number;
            name: string;
        };
        /** @description NOTE(W2): two producers write this body. The three group writes return it from internal/api/v2/projects/groups.go:295-330. The project listing returns an array of it from internal/api/v2/projects/handler.go:134-143 (the Project struct) assembled at :226-251. Both mirror the reference's ProjectListModel, so a client re-renders the group chips without re-fetching. The struct has no `status`, `description` or `role` field: handler_test.go:145-150 pins the eight-key golden body and fails on a non-baseline key. */
        ProjectWithGroups: {
            create_success: boolean;
            groups: components["schemas"]["ProjectGroup"][];
            id: number;
            keycloak_groups: {
                [key: string]: unknown;
            };
            name: string;
            owner_id: number;
            plugins: string[];
            suspended: boolean;
        };
        /** @description Opaque selected-tools blob (object or null) — see marker. */
        SelectedTools: unknown;
        SyncCursorError: {
            error: string;
            message?: string;
        };
        /** @description One removal in a `changes_since` delta. */
        SyncTombstone: {
            /** Format: date-time */
            deleted_at: string;
            /**
             * Format: int64
             * @description The removed item's integer id (for a message, the message group id).
             */
            id: number;
            /**
             * @description `deleted`, or `access_lost` (the item left this list for the caller: made private, the caller removed as a participant, or no longer matching the list filter). Unknown reasons are removals too.
             * @enum {string}
             */
            reason: "deleted" | "access_lost";
            /** @description The removed item's uuid; null only where the server stored none. */
            uuid: string | null;
        };
        /** @description Opaque tool/toolkit settings blob — see marker. */
        ToolSettings: unknown;
        TurnStartError: {
            error: string;
            message?: string;
            retryable?: boolean;
        };
        VersionMeta: {
            /** @description Written by UpdateAttachmentStorage as {"toolkit_id": "..."} (eliteacore/handler.go:1777-1781). */
            attachment_storage?: {
                toolkit_id?: string;
            };
            category?: string;
            icon_meta?: {
                [key: string]: unknown;
            };
            /** @description Identifiers of the agent's enabled internal tools (e.g. "internal_mcp"). Client-owned: elitea-main never inspects the key — UpdateVersion assigns the whole meta map it receives (applications/handler.go:826-828) — but the legacy UI writes it here and the replatformed edit page both reads and writes it, so it is modelled rather than left to the passthrough marker below. */
            internal_tools?: string[];
            /** @description NOTE(#898): the editor's free-text "Editor Notes". It has no column of its own on `application_versions` and is not meant to: pylon stores it inside this same jsonb (elitea_issues issue 5410 chose the blob over a per-tenant schema migration), folding the top-level `notes` write field in (legacy/plugins/elitea_core/utils/application_utils.py:191-211) and lifting it back out on read (models/pd/version.py:297-307). This service does the same, and additionally leaves the key in place here — the agent editor round-trips `meta` wholesale, so deleting it from the read would make an ordinary save of any other field erase the notes. Documentation only: never sent to the model, to chat, or to execution. */
            notes?: string;
            parent_author_id?: string;
            parent_entity_id?: string;
            parent_project_id?: string;
            source_version_id?: string;
            step_limit?: number | null;
            /** @description Written into meta on create when the request carries variables (applications/handler.go:430, 747-752). */
            variables?: components["schemas"]["VersionVariable"][];
        } & {
            [key: string]: unknown;
        };
        /** @description NOTE(W2): tag entries emitted by ExportImportGet (eliteacore/handler.go:2650-2655 — name always a scanned string there) and Fork's response echo (:2431-2438), which builds entries from unvalidated client maps: a fork payload tag without a name yields {"name": null, ...}. Hence name is nullable and not required. NOTE(#345): the same shape is now also the WRITE shape — `tags` on VersionWriteRequest refs this schema, matching pylon's own PromptTagUpdateModel (name, optional data, optional id). */
        VersionTag: {
            data?: unknown;
            /** @description NOTE(#345): the `tags` row this association points at. The applications handler emits it on every version-detail read (versionTags, internal/api/v2/applications/handler.go). The export and fork echoes project name and data only, so it is optional. A write ignores it: the handler matches a tag by name, the same way pylon does (legacy/plugins/elitea_core/utils/application_utils.py:219-226). */
            id?: number;
            name?: string | null;
        };
        /** @description NOTE(W2): two row shapes merged into one `tools` array — entity_tool_mapping rows carry id/tool_id/entity_type/selected_tools, plus the joined elitea_tools payload under `config` in fetchVersionDetails (applications/handler.go:256-271) but under `settings` in GetVersionExpanded (:1101-1114) and publicApplicationDetail (eliteacore/handler.go:1411-1425); application_tools rows carry id/name/type/settings plus author_id in fetchVersionDetails and author_id in GetVersionExpanded, and publicApplicationDetail emits the sub-agent references as their own entries. `project_id` names the project the tool belongs to and is emitted by GetVersionExpanded on EVERY tool and by publicApplicationDetail on the sub-agent entries; it is what lets the runtime say which tenant schema an `application`-type tool refers to. Only `id` is common to all. The IMPORT and the FORK echoes carry a third shape: the import names each link it wrote with `id`, `name` and `type` taken from the toolkit it imported (eliteacore/handler.go, the link phase of ExportImportPost) — the two literals `"custom"` and `""` that stood there named no toolkit at all — and the fork writes an empty list, because it copies no toolkit. On that import path `id` is the `elitea_tools` row and it is serialized as a STRING, unlike the integer the read paths above emit. */
        VersionToolRef: {
            author_id?: string;
            config?: components["schemas"]["ToolSettings"];
            entity_type?: string;
            /** @description entity_tool_mapping.id or application_tools.id */
            id: number;
            name?: string;
            project_id?: number;
            selected_tools?: components["schemas"]["SelectedTools"];
            settings?: components["schemas"]["ToolSettings"];
            tool_id?: number;
            type?: string;
        };
        /** @description NOTE(W2): DB-backed paths always emit both keys as strings (applications/handler.go:318-321; eliteacore ExportImportGet :2629). The echo paths do NOT guarantee that: CreateVersion returns the client's raw variables array verbatim (applications/handler.go: 772-778, 797 — keys may be absent entirely) and Fork rebuilds entries from unvalidated client maps (eliteacore/handler.go:2421-2428 — "name"/"value" marshal as null when missing). Hence no required list and nullable on both. */
        VersionVariable: {
            name?: string | null;
            value?: string | null;
        };
    };
    responses: {
        /** @description Bad request */
        400: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ErrorResponse"];
            };
        };
        /** @description Unauthorized */
        401: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ErrorResponse"];
            };
        };
        /** @description Forbidden */
        403: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ErrorResponse"];
            };
        };
        /** @description Not found */
        404: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ErrorResponse"];
            };
        };
        /** @description Internal server error */
        500: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ErrorResponse"];
            };
        };
        /** @description 410 Gone, `error` = `sync_cursor_expired`: the cursor is older than the server's tombstone retention (97 days). Discard this list's cache and resync with `changes_since=0`. */
        SyncCursorExpired: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["SyncCursorError"];
            };
        };
        /** @description 400. For a delta request, `error` is `invalid_sync_cursor` (the cursor does not decode or belongs to another list, scope or filter: discard it and resync from `0`), `invalid_limit`, or (notifications) `invalid_sync_request`. A legacy request's 400 uses the same `error` key with a message. */
        SyncCursorRefused: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["SyncCursorError"];
            };
        };
    };
    parameters: {
        /** @description Maximum number of items to return */
        Limit: number;
        /** @description Number of items to skip before starting to collect results */
        Offset: number;
        /** @description Project identifier (extracted from URL path) */
        ProjectId: string;
    };
    requestBodies: never;
    headers: never;
    pathItems: never;
}
export type $defs = Record<string, never>;
export interface operations {
    getClientDiscovery: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The document. Strong ETag; If-None-Match answers 304. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["DiscoveryDocument"];
                };
            };
            /** @description Unchanged since the presented entity tag. */
            304: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            /** @description A source of the document is unavailable; retry later. */
            503: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
        };
    };
    revokeNativeToken: {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/x-www-form-urlencoded": components["schemas"]["NativeRevokeRequest"];
            };
        };
        responses: {
            /** @description Revoked, or nothing to revoke. No body. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            /** @description invalid_request. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NativeOAuthError"];
                };
            };
            /** @description invalid_client. */
            401: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NativeOAuthError"];
                };
            };
        };
    };
    exchangeNativeToken: {
        parameters: {
            query?: never;
            header?: {
                "X-Client-Version"?: string;
            };
            path?: never;
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/x-www-form-urlencoded": components["schemas"]["NativeTokenRequest"];
            };
        };
        responses: {
            /** @description A new token pair and the full client policy. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NativeTokenResponse"];
                };
            };
            /** @description invalid_request, unsupported_grant_type or invalid_grant. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NativeOAuthError"];
                };
            };
            /** @description invalid_client, or the flat device_revoked body (the app wipes). */
            401: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NativeOAuthError"] | components["schemas"]["DeviceRevokedError"];
                };
            };
            /** @description No native client is registered on this deployment. */
            404: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NativeOAuthError"];
                };
            };
            /** @description This app version is below the minimum; header X-Min-Client-Version repeats it. */
            426: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ClientUpgradeRequiredError"];
                };
            };
            /** @description Too many failed requests; see Retry-After. */
            429: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NativeOAuthError"];
                };
            };
            /** @description temporarily_unavailable; see Retry-After. */
            503: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NativeOAuthError"];
                };
            };
        };
    };
    getBrandingPackJSON: {
        parameters: {
            query?: {
                /** @description Content version (the unquoted ETag) as the discovery document's brand_pack_url carries it. */
                v?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The pack (the upstream BrandPack schema, asset references made absolute). Header X-Elitea-Brand-Layers lists the contributing layers (default, file, db); `default` alone means the product default rather than a deployment brand. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["BrandPack"];
                };
            };
            /** @description A stale `v`; redirects to the current one (same origin, same path). */
            302: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            /** @description Unchanged since the presented entity tag. */
            304: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
        };
    };
    listConfigurationModels: {
        parameters: {
            query?: {
                include_shared?: boolean;
                section?: string;
            };
            header?: never;
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The model catalogue for this project */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ConfigurationModelListResponse"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            500: components["responses"]["500"];
        };
    };
    getApplication: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                application_id: number;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Application details with version summaries */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationDetail"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
        };
    };
    listApplications: {
        parameters: {
            query?: {
                agents_type?: string;
                folder_id?: string;
                /** @description Maximum number of items to return */
                limit?: components["parameters"]["Limit"];
                /** @description Number of items to skip before starting to collect results */
                offset?: components["parameters"]["Offset"];
                /** @description Search text (preferred over `search` by the handler). */
                query?: string;
                /** @description Comma-separated tag filter. Each entry matches a tag NAME or a tag ID. The entries combine with AND: a listed application carries every named tag on one of its versions. Blank entries are ignored (repos/applications.go splitTagFilter). */
                tags?: string;
            };
            header?: never;
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Paginated application list */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationList"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
        };
    };
    getConversation: {
        parameters: {
            query?: {
                /** @description When > 0, embeds up to this many message groups as `message_groups`. */
                messages_limit?: number;
                sort_order?: "asc" | "desc";
            };
            header?: never;
            path: {
                /** @description The conversation's numeric id or its uuid; the server accepts either. */
                conversation_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The conversation. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ConversationDetail"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
            500: components["responses"]["500"];
        };
    };
    deleteConversation: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The conversation's numeric id or its uuid; the server accepts either. */
                conversation_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Deleted. */
            204: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
            500: components["responses"]["500"];
        };
    };
    listConversations: {
        parameters: {
            query?: {
                /** @description Incremental sync. Absent: the legacy page. Empty or `0`: a full sync in delta shape. Otherwise the `next_cursor` of an earlier delta of the SAME list (same project and same source, entity_name, entity_meta_id, mine and hidden values; another list's cursor is a 400). `offset` is ignored; `limit` defaults to 100 and bounds rows and tombstones each. Losing access to the whole project is a 403, not a tombstone. */
                changes_since?: string;
                /** @description The participant entity's id, paired with entity_name. */
                entity_meta_id?: string;
                /** @description `application` (agents and pipelines) or `toolkit` (toolkits and MCP connections). */
                entity_name?: string;
                /** @description `only` inverts the default hidden-conversation exclusion; any other value keeps the default. */
                hidden?: string;
                limit?: number;
                mine?: boolean;
                offset?: number;
                /** @description Filter by `chat_conversations.source` (e.g. `deepwiki`). */
                source?: string;
            };
            header?: never;
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The matching conversations, newest first. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ConversationListing"];
                };
            };
            400: components["responses"]["SyncCursorRefused"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            410: components["responses"]["SyncCursorExpired"];
            500: components["responses"]["500"];
        };
    };
    createConversation: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["CreateConversationRequest"];
            };
        };
        responses: {
            /** @description The created conversation (without participants or messages). */
            201: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ConversationDetail"];
                };
            };
            400: components["responses"]["400"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            500: components["responses"]["500"];
        };
    };
    listConversationMessages: {
        parameters: {
            query?: {
                /** @description Empty or `0` is a full sync; otherwise the `next_cursor` of an earlier delta of this transcript. */
                changes_since?: string;
                /** @description At most 100 (default 10; 100 in delta mode, for rows and tombstones each). */
                limit?: number;
                offset?: number;
            };
            header?: never;
            path: {
                /** @description The conversation's uuid (a send takes the uuid only; the list also takes the numeric id). */
                conversation_uuid: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The transcript page or delta. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ConversationMessageListing"];
                };
            };
            400: components["responses"]["SyncCursorRefused"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
            410: components["responses"]["SyncCursorExpired"];
            500: components["responses"]["500"];
        };
    };
    startApplicationTurn: {
        parameters: {
            query: {
                execution_contract: "agent.execute.application.v1" | "agent.execute.adhoc.v1";
            };
            header?: never;
            path: {
                /** @description The conversation's uuid (a send takes the uuid only; the list also takes the numeric id). */
                conversation_uuid: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ApplicationTurnRequest"];
            };
        };
        responses: {
            /** @description The execution was started (or an identical earlier send was found). */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationTurnStarted"];
                };
            };
            /** @description Invalid request. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["TurnStartError"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            /** @description The question_id was already used with a different body, or the turn conflicts with server state. */
            409: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["TurnStartError"];
                };
            };
            /** @description The request was not application/json. */
            415: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            /** @description The participant cannot be executed through this contract (`unsupported_agent_execution`). */
            422: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["TurnStartError"];
                };
            };
            /** @description The server is busy; retry after the `Retry-After` header. */
            503: {
                headers: {
                    "Retry-After"?: number;
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["TurnStartError"];
                };
            };
        };
    };
    listNotifications: {
        parameters: {
            query?: {
                /** @description Empty or `0` is a full sync; otherwise the `next_cursor` of an earlier notification delta of the same user. */
                changes_since?: string;
                event_type?: string;
                limit?: number;
                offset?: number;
                only_new?: boolean;
                only_total?: boolean;
                sort_by?: string;
                sort_order?: "asc" | "desc";
            };
            header?: never;
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description One page of notifications (with only_total=true, `rows` is absent), or a delta. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationPage"];
                };
            };
            400: components["responses"]["SyncCursorRefused"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            410: components["responses"]["SyncCursorExpired"];
            500: components["responses"]["500"];
        };
    };
    updateNotifications: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["NotificationSeenUpdate"];
            };
        };
        responses: {
            /** @description The number of notifications changed. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationSeenUpdated"];
                };
            };
            400: components["responses"]["400"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            500: components["responses"]["500"];
        };
    };
    listProjects: {
        parameters: {
            query?: {
                /** @description Presence-only flag. The handler ignores the value, so `check_public_role=false` also switches the check on. */
                check_public_role?: boolean;
            };
            header?: never;
            path: {
                public_project_id: number;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Accessible projects */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ProjectWithGroups"][];
                };
            };
            401: components["responses"]["401"];
        };
    };
}
