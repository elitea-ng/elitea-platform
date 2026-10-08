/**
 * GENERATED FILE: do not edit. Regenerate with `npm run contract:gen`.
 *
 * Types derived from the pinned server contract (contract/elitea-v2.yaml,
 * Apache-2.0, see contract/LICENSE.elitea) merged with the Agent Zefir
 * native-gap overlay and pruned to contract/client-operations.json.
 *
 * upstream sha256:    4b0307162a8f95fb401f64dfb4c71eb8ac2abde2dacdb06502b72d7a26ca21b1
 * overlay sha256:     150a9e084a8afb335b2a43841256b5be5399f5514dd438d01ee437d2604c992f
 * operations sha256:  c350fd90f6b306c1b70751f6f4b3a5128a8fe54bb5aeae6180a4793b31eff1f9
 */
export interface paths {
    "/.well-known/elitea-client": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Public discovery document for clients
         * @description What a client given only a deployment's origin needs before it signs
         *     in (ADR-0025 decision 1): the server version (`major.minor`), the
         *     client contract version, the deployment kind, the brand's display
         *     name and pack URL, the native authorization endpoints (`null` while
         *     no native client is registered), the public client policy and the
         *     minimum client version per registered client id. It discloses nothing
         *     the deployment's login page does not.
         *
         *     Caching: strong `ETag` (hash of the body), `Cache-Control: no-cache`
         *     (the body follows database state), `If-None-Match` answers 304. When
         *     the deployment has no configured public origin the URLs are built
         *     from the request's Host and the response carries
         *     `Vary: Host, X-Forwarded-Proto`. A failing source answers 503
         *     `no-store` rather than a document that would wrongly say "no native
         *     sign-in".
         */
        get: operations["getClientDiscovery"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/auth/native/devices": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * List the caller's native devices
         * @description The caller's own device sessions (refresh-token families, ADR-0025
         *     decision 4), newest first. `current` marks the device whose token
         *     authenticated this request. Revoked devices are listed only with
         *     `include_revoked=true`. Answers 404 while no native client is
         *     registered. NOTE(W2): internal/api/nativeauth/devices.go:90.
         */
        get: operations["listNativeDevices"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/auth/native/devices/{device_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                device_id: string;
            };
            cookie?: never;
        };
        get?: never;
        put?: never;
        post?: never;
        /**
         * Revoke one of the caller's native devices
         * @description Revokes the device session (reason `user`). Another user's device, or
         *     an unknown id, is a 404, so ownership is not disclosed. Revoking the
         *     device in use is allowed; its next call answers `device_revoked`.
         *     NOTE(W2): internal/api/nativeauth/devices.go:114.
         */
        delete: operations["revokeNativeDevice"];
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
         * Revoke a native device session (RFC 7009)
         * @description Revokes the WHOLE device session (refresh-token family) the presented
         *     refresh or access token belongs to (reason `signed_out`). An unknown or
         *     already-revoked token, or one for another client, also answers 200 with
         *     no body, so the answer reveals nothing. Form body: `token`,
         *     `client_id`, optional `token_type_hint`. Answers 404 while no native
         *     client is registered. NOTE(W2): internal/api/nativeauth/token.go:277.
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
         * Exchange a native authorization code or refresh token
         * @description The token endpoint for registered native PUBLIC clients (RFC 6749
         *     section 4.1.3 and 6, ADR-0025 decision 3). No client authentication:
         *     `client_id` is in the body and an `Authorization` header is refused.
         *     The body is `application/x-www-form-urlencoded` (at most 8 KiB) and no
         *     parameter may repeat.
         *
         *     `grant_type=authorization_code` takes `code`, `redirect_uri`,
         *     `client_id` and `code_verifier` (PKCE S256). A code is single-use; a
         *     replayed code revokes the device session it produced.
         *     `grant_type=refresh_token` takes `refresh_token`, `client_id` and an
         *     optional `client_version`. The refresh token rotates on every use.
         *     Presenting the IMMEDIATELY previous refresh token again within the
         *     re-delivery window (default 30 seconds) returns the SAME new pair; any
         *     other reuse revokes the whole device session.
         *
         *     Errors are RFC 6749 section 5.2 JSON. A revoked, expired, deactivated
         *     or reused device session answers 401 with the flat
         *     `{"error":"device_revoked"}` body (the client wipes its local data);
         *     clients must persist the new pair before using it and serialise
         *     refreshes. Failures are rate limited per
         *     (client, address) and per address (429 with Retry-After). Answers 404
         *     while no native client is registered.
         *
         *     A client states its version in `X-Client-Version` (a refresh grant's
         *     `client_version` is used when the header is absent and it parses).
         *     Below the minimum for `client_id` the endpoint answers 426 BEFORE the
         *     code or refresh token is consumed, so the same credential works once
         *     the app is updated. Every 200 carries the full `client_policy`.
         *     NOTE(W2): internal/api/nativeauth/token.go:47.
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
        /**
         * The resolved brand pack as JSON, with absolute references
         * @description Serves the same resolved brand pack as `GET /branding/bootstrap.js`
         *     (one resolver), as JSON, with every same-origin reference
         *     (`assets.*`, `typography.fontFaces[].url`, `product.docsUrl`) made
         *     absolute against the deployment's public origin (`DEPLOYMENT_URL`).
         *     Unauthenticated by design: a native client brands its sign-in screen
         *     with it.
         *
         *     On a deployment with no brand pack of its own the response is the
         *     product default pack (200), and `X-Elitea-Brand-Layers` says
         *     `default`. That header lists the contributing layers bottom first:
         *     `default` or `file`, then `db` when the admin Branding section
         *     contributed.
         *
         *     Caching follows bootstrap.js, with pack.json's OWN entity tag: the
         *     strong `ETag` is the hash of this response body (which carries the
         *     public origin and, unbranded, the product default, neither of which
         *     is in bootstrap.js's tag); `no-cache` on the bare URL;
         *     `public, max-age=31536000, immutable` when `?v=` equals the current
         *     ETag value; a stale `?v=` answers 302 to the current URL;
         *     `If-None-Match` answers 304. When the deployment has no configured
         *     public origin the absolute URLs are built from the request's Host,
         *     the response carries `Vary: Host, X-Forwarded-Proto`, and it is
         *     never immutable.
         */
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
    "/elitea_core/application_skills/prompt_lib/{project_id}/{app_version_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                app_version_id: number;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        /**
         * List skills attached to an agent version
         * @description Returns only the skills attached to app_version_id, read from
         *     entity_skill_mapping.
         *
         *     NOTE(#367): this route used to point at the project skills List
         *     handler, which never read app_version_id and answered with every
         *     skill in the project, at 200. The five published keys are unchanged —
         *     a client sees the same shape and different, correct contents. A
         *     malformed app_version_id is now refused with 400 instead of being
         *     coerced.
         *
         *     NOTE(issue 621): the response schema is ApplicationSkillsList, not
         *     SkillsList. It is SkillsList plus the `skills` and `max_skills` keys
         *     the reviewed handler also emits (issue 395). SkillsList itself stays
         *     as it is, because the project-skills List operation shares it and
         *     emits neither key.
         */
        get: operations["listApplicationSkills"];
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
    "/elitea_core/attachments/prompt_lib/{project_id}/{conversation_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The conversation UUID. A numeric id is refused with 400: the file is stored as `{conversation_uuid}/{file name}`. */
                conversation_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Upload a file to attach to the next chat message
         * @description `multipart/form-data` with the bytes in `file`. A small file is one
         *     request (201). A large one is split into chunks sent with the same
         *     `file_id` and `file_name` and their `chunk_index`/`total_chunks`;
         *     every chunk but the last answers 202, the last answers 201 with the
         *     assembled file. Send the returned `filepath` in the chat send's
         *     `payload.attachments`. Size limits are the project's storage policy
         *     (images and documents have separate ceilings); one chunk is at most a
         *     few MB.
         */
        post: operations["uploadConversationAttachment"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/attachments/prompt_lib/{project_id}/{conversation_id}/{name}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The conversation UUID (a numeric id is a 400), as in the attachment's `filepath`. */
                conversation_id: string;
                /** @description The file name: the last segment of the attachment's `filepath` (`/{bucket}/{conversation_uuid}/{name}`), percent-encoded. A name with a slash, a backslash, `..` or a control character is a 400. */
                name: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        /**
         * Download one file that was attached in a conversation
         * @description Needs `models.chat.conversation.details` and access to the
         *     conversation — the same as getConversation. A caller who cannot see
         *     the conversation gets 404, as for a file that does not exist. The
         *     bucket is the project's chat attachment bucket; the client never
         *     names it. HEAD answers the same headers without the body.
         *
         *     The answer is always a download: `Content-Disposition: attachment`
         *     (with an RFC 5987 `filename*`), `X-Content-Type-Options: nosniff`,
         *     `Content-Security-Policy: sandbox` and
         *     `Cache-Control: private, no-store`. `Content-Type` is the recorded
         *     type only when the file's first bytes agree with it; an active type
         *     (HTML, SVG, XML, script) or a disagreement is
         *     `application/octet-stream`. One `Range` (`bytes=a-b`, `bytes=a-` or
         *     `bytes=-n`) answers 206; `If-None-Match` with the `ETag` answers 304.
         *     NOTE(W2): internal/api/v2/conversations/attachment_download.go:59
         *     (`DownloadAttachment`).
         */
        get: operations["downloadConversationAttachment"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/continue_predict/prompt_lib/{project_id}/{conversation_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The conversation UUID; must equal the body's `conversation_uuid`. */
                conversation_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Resume a paused turn (human decision, tool authorization, output limit)
         * @description Answers a pause the event stream announced and starts the turn's
         *     continuation, answering like the send with a new `events_url`.
         *     `execution_contract`:
         *     - `agent.continue.hitl.v1` — a human-in-the-loop decision
         *       (`hitl_resume: true` plus `hitl_action`/`hitl_value`, or
         *       `hitl_decisions` for a multi-pause frame);
         *     - `agent.continue.authorization.v1` — a toolkit/MCP authorization
         *       answer (`authorization_request_id` + `authorization_action`, or
         *       batched `hitl_decisions`);
         *     - `agent.continue.output-limit.v1` — continue an answer cut at the
         *       model's output limit (`message_id` only).
         *     A pause that was already answered is a 409 with
         *     `agent_hitl_already_resolved`, `agent_authorization_already_resolved`
         *     or `agent_output_limit_already_resolved` and `retryable: false`:
         *     refresh the transcript instead of retrying.
         *
         *     The existing static continuation contract uses agent.continue.static.v1 with the exact original pause identity.
         */
        post: operations["continueChatExecution"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/conversation_export/prompt_lib/{project_id}/{conversation_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The conversation's numeric id or its UUID. Both resolve, the same way the conversation detail read resolves them. */
                conversation_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        /**
         * Download one conversation's transcript as a document
         * @description The rail's row menu offers this as "Export", with one entry per format.
         *     Both answers carry `Content-Disposition: attachment` and a filename
         *     derived from the conversation's own name, so a browser saves the
         *     response rather than rendering it.
         *
         *     The document is the WHOLE transcript, oldest message first, never a
         *     page of it. A conversation with more messages than the server will
         *     assemble in one document is refused with 400 rather than exported
         *     truncated: a partial file that looks complete is the failure this
         *     refusal exists to prevent.
         *
         *     Reading an export needs the same permission as reading the messages
         *     (`models.chat.messages.list`). There is no separate export right.
         */
        get: operations["exportConversation"];
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
                /** @description The conversation's numeric id or its UUID; both resolve. */
                conversation_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        /**
         * One conversation with its participants
         * @description Needs `models.chat.conversation.details` and access to the
         *     conversation itself (a private conversation is visible only to the
         *     callers the chat authority admits; anyone else gets 403 or 404).
         *     `messages_limit` > 0 embeds the newest message groups in the legacy
         *     projection; a client that keeps a transcript reads the message list
         *     instead.
         */
        get: operations["getConversation"];
        /**
         * Rename a conversation, or change its privacy, folder or settings
         * @description Client contract 1.4. Needs `models.chat.conversation.update` and
         *     access to the conversation itself (the chat authority's detail
         *     rule, the same one the web app's rename passes); a conversation the
         *     caller cannot see answers 404.
         *
         *     The route writes only the keys the body states. A body with `name`
         *     alone renames the conversation and leaves its privacy, folder,
         *     instructions and settings as they are. `name` is trimmed; a name that
         *     is not a string, is blank after trimming, is longer than 256
         *     characters or contains a control character answers 400 and writes
         *     nothing. `null` or an absent `name` does not rename.
         *
         *     A rename reaches the caller's other devices through the
         *     `changes_since` delta of the conversation list (the row comes back
         *     with its new `name` and `updated_at`).
         */
        put: operations["updateConversation"];
        post?: never;
        /**
         * Delete a conversation and its stored attachments
         * @description Removes the conversation, every message group and the bytes of every
         *     attachment its messages carried. A client that syncs sees ONE
         *     `deleted` tombstone for the conversation on the conversation list (no
         *     per-message tombstones). A 500 after the rows are gone means the byte
         *     cleanup failed; the conversation is still deleted.
         */
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
         *     The route also serves POST, createConversation below.
         */
        get: operations["listConversations"];
        put?: never;
        /**
         * Create a conversation
         * @description Creates a conversation owned by the caller, private unless
         *     `is_private: false` is sent, with the caller's personalization and the
         *     project's chat defaults applied. Answers 201 with the stored
         *     conversation; its `uuid` is the id the chat send and the attachment
         *     upload take.
         */
        post: operations["createConversation"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/memories/prompt_lib/{project_id}": {
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
         * List the caller's own persistent, cross-conversation memories
         * @description internal/api/v2/memories/handler.go's List (#870). Answers only the
         *     AUTHENTICATED caller's own entries in this project — never another
         *     user's, and never gated by an "admin sees everyone's memories" mode.
         *     Most recent first. `q`, when given, filters by a case-insensitive
         *     substring match over content and tags.
         */
        get: operations["listMemories"];
        put?: never;
        post?: never;
        /**
         * Delete every one of the caller's own memories in this project
         * @description internal/api/v2/memories/handler.go's ClearAll (#870). Settings >
         *     Memory's "Clear all" control, after its own confirmation dialog.
         *     Never touches another user's memories.
         */
        delete: operations["clearMemories"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/memory/prompt_lib/{project_id}/{memory_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                memory_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        get?: never;
        put?: never;
        post?: never;
        /**
         * Delete one of the caller's own memories
         * @description internal/api/v2/memories/handler.go's Delete (#870). Idempotent: a
         *     `memory_id` that never existed, or already belonged to someone else,
         *     answers 204 the same as a successful delete — the same
         *     DeleteMessageFeedback-style shape, matching MemoriesRepo.Delete's own
         *     comment.
         */
        delete: operations["deleteMemory"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/message_feedback/prompt_lib/{project_id}/{message_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The message-GROUP uuid — the same `{messageID}` GetMessage and DeleteMessage share (internal/api/v2/conversations/handler.go). */
                message_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        /**
         * Aggregate like/dislike counts for one message, plus the caller's own vote
         * @description internal/api/v2/conversations/handler.go's GetMessageFeedback (#880).
         *     Reads p_{project_id}.chat_message_feedback
         *     (tenant/0135_chat_message_feedback.sql). `mine` is absent when the
         *     caller has not rated this message.
         */
        get: operations["getMessageFeedback"];
        put?: never;
        /**
         * Like or dislike a message, with an optional comment
         * @description internal/api/v2/conversations/handler.go's SetMessageFeedback (#880).
         *     Upserts into p_{project_id}.chat_message_feedback — a second call
         *     from the same user REPLACES their rating and comment rather than
         *     adding a second row (UNIQUE(message_group_uuid, user_id)). Returns
         *     the updated aggregate, same shape as the GET.
         */
        post: operations["setMessageFeedback"];
        /**
         * Retract the caller's own like/dislike on a message
         * @description internal/api/v2/conversations/handler.go's DeleteMessageFeedback
         *     (#880). Removes only the CALLER's own row; other users' feedback on
         *     the same message is untouched. Returns the updated aggregate.
         */
        delete: operations["deleteMessageFeedback"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/message_trace/prompt_lib/{project_id}/{step_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
                step_id: number;
            };
            cookie?: never;
        };
        /**
         * One trace step in full, fetched on pin expand
         * @description The light projection plus the heavy fields.
         *
         *     `message_group_id` is REQUIRED and is matched together with the step
         *     id: it is what confines a numeric step id to the message the caller is
         *     reading, so a step from elsewhere is a 404 rather than a body.
         */
        get: operations["getMessageTrace"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/message_traces/prompt_lib/{project_id}/{conversation_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                conversation_id: number;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        /**
         * Trace steps for a conversation — the pin strip under each answer
         * @description The LIGHT projection: labels, ordering and the bounded `attrs` display
         *     sidecar. The heavy fields (tool_inputs, tool_output, text, thinking)
         *     are detail-only and are fetched one step at a time through
         *     getMessageTrace, so rendering a conversation's chips never detoasts
         *     them.
         *
         *     Rows are ordered by (started_at, id) with null timestamps last. Blank
         *     thinking steps — transition markers the runtime emits with an action
         *     but no text — are excluded, because the client draws a pin for every
         *     row it receives.
         *
         *     `total` is null unless include_total=true, so "not counted" stays
         *     distinguishable from "counted zero".
         */
        get: operations["listMessageTraces"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/messages/prompt_lib/{project_id}/{conversation_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The conversation. The list takes its numeric id or its UUID; the send takes the UUID only, and it must equal the body's `conversation_uuid`. */
                conversation_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        /**
         * A conversation's transcript, paged or as a delta
         * @description Legacy mode pages message groups newest first (`limit`, default 10,
         *     at most 100; `offset`). With `changes_since` (ADR-0025 WP6) it answers
         *     the groups changed since the cursor, oldest change first, the
         *     tombstones of deleted groups and the next cursor; `offset`, `page`,
         *     `sort_by`, `sort_order` and `query` are then ignored. A group that is
         *     still streaming changes again when it settles, so a delta returns it
         *     again; upsert by `id`.
         */
        get: operations["listConversationMessages"];
        put?: never;
        /**
         * Send a chat message and start the answering turn
         * @description Admits one turn and answers at once with the turn's `events_url`; the
         *     answer streams on that URL (streamExecutionEvents) and is stored in
         *     the transcript as it settles.
         *
         *     IDEMPOTENCY. `question_id` is required, client generated and a
         *     LOWERCASE canonical UUID. It is the admission idempotency key:
         *     - the same `question_id` with the same body replays the original
         *       admission — 200 with `created: false` and the SAME `execution_id`,
         *       `response_message_id` and `events_url`;
         *     - the same `question_id` with a different body answers 409
         *       (`Agent execution request conflicts with an existing turn`).
         *     So a client that lost the response (timeout, dropped connection)
         *     resends the identical request and follows the `events_url` it gets
         *     back; it never mints a new `question_id` for a retry.
         *
         *     `execution_contract` selects the turn kind:
         *     `agent.execute.application.v1` (an agent or pipeline participant,
         *     `participant_id` > 0) or `agent.execute.adhoc.v1` (a model chat,
         *     `llm_settings` required). A body the current execution path cannot
         *     run answers 422 `unsupported_agent_execution`. While the previous
         *     answer of this conversation is still settling, the server waits up to
         *     about 3 s before it decides.
         */
        post: operations["sendChatMessage"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/participant_candidates/prompt_lib/{project_id}/{conversation_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The conversation's numeric id or its UUID; both resolve. */
                conversation_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        /**
         * The people who may be added to a conversation
         * @description Members of the project (platform system accounts and suspended users
         *     excluded), filtered by `q` and paged by `cursor`. Needs
         *     `models.chat.participants.create` — the permission of the add this
         *     list feeds — and access to the conversation. `already_participant`
         *     marks members who are participants of THIS conversation already
         *     (the caller is usually one). Ordered by display name, then user id;
         *     the order is stable, so a cursor never repeats or skips a member.
         *     Add a candidate with `addConversationParticipants` and
         *     `{entity_name: user, entity_meta: {id: user_id, user_name: name}}`.
         *     NOTE(W2): internal/api/v2/conversations/candidates.go:83
         *     (`ListParticipantCandidates`).
         */
        get: operations["listParticipantCandidates"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/participant/prompt_lib/{project_id}/{conversation_id}/{participant_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The conversation's numeric id or its UUID; both resolve. */
                conversation_id: string;
                /** @description The participant row id (`ConversationParticipant.id`), not the entity id. */
                participant_id: number;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        get?: never;
        put?: never;
        post?: never;
        /**
         * Remove a participant from a conversation
         * @description Needs `models.chat.participant.delete` and access to the
         *     conversation. The conversation's author cannot be removed (400). An
         *     id that is not a participant of this conversation is 404.
         *     NOTE(W2): internal/api/v2/conversations/handler.go:1399
         *     (`RemoveParticipant`).
         */
        delete: operations["deleteConversationParticipant"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/participants/prompt_lib/{project_id}/{conversation_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The conversation's numeric id or its UUID; both resolve. */
                conversation_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Add participants (people, agents, pipelines, toolkits, models) to a conversation
         * @description Needs `models.chat.participants.create` and access to the
         *     conversation. The body is ONE participant object or an array of at
         *     most 100. The batch is all-or-nothing.
         *
         *     Per kind (`entity_name`):
         *     - `user`: `entity_meta.id` is the USER id (> 0). Send
         *       `entity_meta.user_name` too; it becomes the participant's
         *       `meta.user_name`, which is what transcripts show.
         *     - `application` and `pipeline`: `entity_meta.id` (> 0) and
         *       `entity_meta.project_id` (> 0; defaults to the path project), plus
         *       `entity_meta.name` for display. Send `entity_settings.version_id`:
         *       a chat send to an application participant runs that version, and a
         *       participant without one cannot be targeted by a send.
         *     - `toolkit`: `entity_meta.id` and `entity_meta.project_id`, as above.
         *       A conversation that holds a toolkit AND an agent is refused at send
         *       time with 422 (`agent_chat.sql`); a toolkit belongs in a model
         *       (ad-hoc) conversation.
         *     - `llm`: `entity_meta.model_name`.
         *     - `dummy`: no keys.
         *
         *     The answer is the participant rows of THIS request, in request
         *     order. An entity that is already a participant is not added twice:
         *     its existing row (and id) is answered again, so a retried add is
         *     safe. NOTE(W2): internal/api/v2/conversations/handler.go:1297
         *     (`AddParticipant`).
         */
        post: operations["addConversationParticipants"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/public_application/prompt_lib/{application_id}/{version_name}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                application_id: number;
                version_name: string;
            };
            cookie?: never;
        };
        /**
         * Retrieve a published application by public name and version label
         * @description NOTE(W2): internal/api/v2/eliteacore/handler.go:1319-1483.
         */
        get: operations["getPublicApplication"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/public_applications/prompt_lib": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * List published applications visible without project membership
         * @description The public catalogue. Answers `{rows, total}` where `total` counts the
         *     FILTERED set, not the whole catalogue, so a client can page through it.
         *
         *     Every parameter below is read by
         *     internal/api/v2/eliteacore/public_applications.go. A `sort_by`,
         *     `sort_order`, `agents_type` or `statuses` value outside its allowlist,
         *     or a `limit`/`offset` outside its range, is refused with 400 and a body
         *     naming the parameter (InvalidQueryParameterError) — it is never
         *     silently ignored. A native client whose `X-Client-Version` does not
         *     parse gets the same status and the same `{error, error_description}`
         *     keys, with `error: invalid_client_version`.
         *
         *     Trending (`trend_start_period`/`trend_end_period`) is the one pylon
         *     parameter still missing here.
         */
        get: operations["listPublicApplications"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/recommendations/prompt_lib/{project_id}": {
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
         * Get recommended applications for a project
         * @description NOTE(W2): internal/api/v2/eliteacore/handler.go:1705-1734 —
         *     {applications, total} envelope (NOT a bare array); limit/days are
         *     accepted but not read (hard LIMIT 10).
         */
        get: operations["getRecommendations"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/regenerate/prompt_lib/{project_id}/{message_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description The assistant answer to regenerate (its `uid`, the send's `response_message_id`). */
                message_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Regenerate an answer, optionally from an edited question
         * @description Starts a turn that rewrites the answer in place and answers like the
         *     send. `regeneration_id` (client generated, lowercase canonical UUID)
         *     is the idempotency key: the same id replays the admission, so a retry
         *     reuses it. While the previous answer is still being finalised the
         *     route answers 409 `agent_regeneration_pending` with `Retry-After: 1`
         *     and `retryable: true`.
         */
        post: operations["regenerateChatMessage"];
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/task/prompt_lib/{project_id}/{response_message_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
                /** @description The running answer to stop (its `uid`, the send's or the regeneration's `response_message_id`). */
                response_message_id: string;
            };
            cookie?: never;
        };
        get?: never;
        put?: never;
        post?: never;
        /**
         * Stop a running answer on the server
         * @description Client contract 1.3. Stops the turn that is writing this answer: the
         *     execution's desired state becomes CANCELLED, which both workers
         *     observe (the Python worker on its next lease poll or output
         *     acknowledgement, after any synchronous SDK step already running; the
         *     Rust worker through its lease monitor, which stops the model run) and
         *     settle as cancelled. Dropping the stream alone does
         *     NOT stop the run; it keeps spending the budget.
         *
         *     What happens to the answer: a turn stopped before any output is
         *     removed together with its question; a turn with partial output keeps
         *     what was written. After a 204, close the stream and reload the
         *     transcript rather than waiting for a terminal frame.
         *
         *     Only the conversation's author, or the user who asked the question,
         *     may stop it (internal/api/v2/agentexecution/cancel.go). The project
         *     permission check runs before the route reads anything, so an
         *     invalid project id answers 403, like a project the caller cannot
         *     use.
         *
         *     Idempotent for the same caller: stopping an answer that this caller
         *     already stopped answers 204 again, also after the run has settled.
         *     The response message id may be sent in either case; the server folds
         *     it to the lowercase spelling it stored.
         *     An answer that completed (or failed) on its own, one that is not this
         *     caller's, or one that does not exist answers 409 without saying
         *     which; for a completed answer the transcript already holds it.
         */
        delete: operations["cancelChatExecution"];
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/toolkit_available_tools/prompt_lib/{project_id}/{toolkit_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
                /** @description Saved toolkit ID in the authorized project. */
                toolkit_id: number;
            };
            cookie?: never;
        };
        /**
         * Discover tools and argument schemas from one saved toolkit
         * @description Main reads the saved toolkit with the authenticated actor's access.
         *     Main freezes configuration references and operator guardrails before admission.
         *     The worker discovers tools from those immutable inputs.
         *     The response contains the accepted immutable result artifact.
         *     Discovery is unavailable until the runtime activation flag is enabled.
         */
        get: operations["listToolkitAvailableTools"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/tools/prompt_lib/{project_id}": {
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
         * List toolkit (tool) instances for a project
         * @description NOTE(W2): internal/api/v2/toolkits/handler.go:512-535 (List) calling
         *     pgRepo.ListToolkits (:811-860). limit/offset are read from the query
         *     string with their OWN clamp — out-of-[1,100] resets to 20
         *     (handler.go:518-520), narrower than the shared Limit parameter's
         *     documented [1,1000]/default 20; offset<0 resets to 0 (:521-523).
         *     The caller must be a member of {project_id} (or a super-admin), and a
         *     repository failure is returned as a safe 500 error.
         */
        get: operations["listToolkitInstances"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/elitea_core/usage/prompt_lib/{project_id}/usage": {
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
         * Current-period usage against the applicable budget
         * @description Backs Settings → Usage. The period is the current calendar month and is
         *     reported back in `period_start`, `period_end` and `resets_at`, so no
         *     date range is accepted.
         *
         *     `scope=project` reports the whole project; `scope=user` reports only the
         *     calling member's own budget within it.
         *
         *     The dimensional views — `daily`, `models` and the token totals — come
         *     from the per-request usage ledger added by issue #320. They are ABSENT,
         *     not empty, when the ledger holds no row for the scope in this period:
         *     an empty series reads as "no calls were made", which is a different and
         *     wrong claim about a deployment upgraded part-way through a period.
         *     `usage_events_available` says which case the payload is in, exactly as
         *     `spend_available` distinguishes "no spend yet" from "no data".
         *
         *     A member who may not see platform cost figures receives the same payload
         *     with `monthly_limit`, `effective_limit`, `spend`, `remaining` and
         *     `currency` REMOVED and `can_see_amounts: false`; the `spend` inside each
         *     `daily` and `models` row is removed too, because leaving the same money
         *     in a per-day series would make the control decorative. Percentages, the
         *     warning threshold, the period and the token counts are still present, so
         *     the usage bar and the charts still render. Project admins, and the owner
         *     of a personal project, see the amounts.
         */
        get: operations["getProjectUsage"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/notifications/notification/prompt_lib/{project_id}/{notification_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                notification_id: number;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        get?: never;
        /**
         * Mark one of the caller's notifications seen
         * @description Takes no body and answers the notification. An id that is not the
         *     caller's answers 400 "Notification is not found" (not 404).
         */
        put: operations["markNotificationSeen"];
        post?: never;
        /**
         * Delete one of the caller's notifications
         * @description Deletes one notification of the CALLER. The deletion reaches the caller's other devices as a `deleted` tombstone in the `changes_since` delta of listNotifications (shared migration 0144).
         */
        delete: operations["deleteNotification"];
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
         * The caller's notifications, paged or as a delta
         * @description The caller's own notifications (the project in the path is the
         *     permission scope). Legacy mode pages them (`limit` 1..1000, default
         *     10; `offset`; `only_new`; `search`; `event_type`; `sort_by`,
         *     `sort_order`; `only_total=true` answers `total` alone). With
         *     `changes_since` (ADR-0025 WP6) it answers the notifications changed
         *     since the cursor, oldest change first, plus tombstones; combining it
         *     with `offset`, `only_new`, `only_total`, `search`, `event_type`,
         *     `sort_by` or `sort_order` is a 400 `invalid_sync_request`.
         */
        get: operations["listNotifications"];
        /** Mark several (or all) of the caller's notifications seen or unseen */
        put: operations["markNotificationsSeen"];
        post?: never;
        /**
         * Delete several of the caller's notifications
         * @description Deletes the listed notifications of the CALLER; an id that is not the caller's is skipped, and `deleted` counts what was removed. Each deletion reaches the caller's other devices as a `deleted` tombstone in the `changes_since` delta of listNotifications (shared migration 0144's AFTER DELETE trigger), so a dismissed item does not come back on another device.
         */
        delete: operations["deleteNotifications"];
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
    "/social/author": {
        parameters: {
            query?: never;
            header?: never;
            path?: never;
            cookie?: never;
        };
        /**
         * Get the authenticated user's own social/author profile
         * @description NOTE(W2): internal/api/v2/social/handler.go:51-123 (GetAuthor). Reads
         *     centry.social_users joined to auth_core__user; on no matching row or
         *     any query error, falls back to a defaulted response built from the
         *     auth context (:98-107) — still 200, never surfaced as an error.
         *
         *     Client contract 1.2: this is the client's "who am I" read. `id` is
         *     the caller's user id, `name` the display name, `email`, `avatar` (a
         *     URL, absolute or relative to the deployment origin such as
         *     `/avatars/{project_id}/{file}`; "" when none is set) and `personal_project_id` — the project
         *     a client opens by default. `personal_project_id` is "" while the
         *     account's personal project is still being provisioned (a fresh
         *     account's first read starts it); a client polls until it is set.
         */
        get: operations["getCurrentAuthor"];
        put?: never;
        post?: never;
        delete?: never;
        options?: never;
        head?: never;
        patch?: never;
        trace?: never;
    };
    "/social/pin/prompt_lib/{project_id}/{entity_type}/{entity_id}": {
        parameters: {
            query?: never;
            header?: never;
            path: {
                entity_id: number;
                entity_type: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        get?: never;
        put?: never;
        /**
         * Pin an entity for the project
         * @description Store one shared pin per project, entity type, and entity identifier.
         *     Repeated requests update the last pinner and timestamp.
         *     Conversation pins require the caller's existing chat detail access.
         *     NOTE(W2): internal/infra/db/repos/social_pins.go:73 implements the shared
         *     centry.social_pins key defined by migrations/shared/0064_centry_social_pins.sql.
         *
         *     Client contract 1.4 (entity_type `conversation`, entity_id the
         *     conversation's numeric id): the pin is the project's, shared by its
         *     members, and shows as `is_pinned` on the conversation's list row. A
         *     new pin re-delivers that row in the `changes_since` delta of every
         *     member who can see the conversation, so a pin set in the web app
         *     reaches the caller's devices and the other way round. A conversation
         *     the caller cannot see answers 404 and pins nothing.
         */
        post: operations["pinEntity"];
        /**
         * Remove a shared project pin
         * @description Remove the project pin regardless of which member last pinned it.
         *     Repeated requests succeed. Conversation access is required before removal.
         *     NOTE(W2): internal/infra/db/repos/social_pins.go:137 uses the canonical shared key.
         *
         *     Client contract 1.4: removing a conversation pin re-delivers the
         *     conversation's list row (`is_pinned: false`) in the `changes_since`
         *     delta; a repeated unpin removes nothing and re-delivers nothing.
         */
        delete: operations["unpinEntity"];
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
        /** @description NOTE(W2): internal/api/v2/applicationskills/handler.go:45-51, CurrentApplicationSkill. */
        ApplicationSkillAttachment: {
            description: string;
            icon_meta?: components["schemas"]["ProjectIconMeta"];
            name: string;
            skill_id: number;
            /** @description NULL when the attachment names a version the skill no longer has. Read it together with `version_missing`. */
            version_id?: number | null;
            /** @description True when entity_skill_mapping points at a version that is gone. The row is still listed, because hiding it would report the agent as having no skill at all. */
            version_missing: boolean;
            version_name: string;
        };
        /**
         * @description NOTE(issue 621): ONE body answers two clients on this path, and the deployment decides which half is real.
         *     `items`, `total`, `page`, `page_size` and `total_pages` are the published contract, which apps/elitea-web reads through shared/api/unwrap.ts. `skills` and `max_skills` are the shape apps/elitea-ui reads. The reviewed route (internal/api/v2/applicationskills, currentApplicationSkillsResponse) has emitted all seven keys since issue 395 — before it carried the first five, turning ELITEA_APPLICATION_SKILLS_ENABLED on gave the web client a body with no `items` key, which unwrapList reports as an unrecognised shape and renders as "no skills".
         *     The compatibility handler this route replaces emits the five keys only. A client reads the union.
         */
        ApplicationSkillsList: components["schemas"]["SkillsList"] & {
            /** @description How many skills one agent version may attach. */
            max_skills?: number;
            /** @description The attachment view of the SAME rows `items` carries. The two halves are built from one row set and cannot disagree. */
            skills?: components["schemas"]["ApplicationSkillAttachment"][];
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
        /** @description One file sent with a message (`item_type` `attachment_message`). `item_details.filepath` is `/{bucket}/{conversation_uuid}/{name}`; download it with downloadConversationAttachment using the last segment as the name. */
        AttachmentMessageItem: {
            id: number;
            item_details: components["schemas"]["AttachmentMessageItemDetails"];
            /** @description `attachment_message`. */
            item_type: string;
            order_index: number;
        } & {
            [key: string]: unknown;
        };
        AttachmentMessageItemDetails: {
            /** @description `image` or `document`; read it, do not enumerate it. */
            attachment_type: string;
            bucket: string;
            /** @description The content chunks the model was given: a `{type: text, text}` header chunk and, for an image embedded for the model, `{type: image_url, image_url: {url: data:...}}` — a client may render that data URL as a thumbnail without a download. Empty when nothing was recorded. */
            content: {
                [key: string]: unknown;
            }[];
            /** @description `/{bucket}/{name}` — the value a chat send's `payload.attachments[].filepath` carried. */
            filepath: string;
            id: number;
            item_type: string;
            /** @description The stored object name, `{conversation_uuid}/{file name}`. */
            name: string;
        } & {
            [key: string]: unknown;
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
        /**
         * @description The calendar month a budget accrues over. Derived exactly as the LLM
         *     gateway derives it (first instant of the current month, UTC) — the
         *     accumulator's key includes the period start, so a period computed any
         *     other way would look up a row that does not exist and report zero spend
         *     against a real limit.
         */
        BudgetPeriodWindow: {
            /**
             * @description The period tag, YYYYMM.
             * @example 202608
             */
            period: string;
            /**
             * @description Last day of the period, YYYY-MM-DD.
             * @example 2026-08-31
             */
            period_end: string;
            /**
             * @description First day of the period, YYYY-MM-DD.
             * @example 2026-08-01
             */
            period_start: string;
            /**
             * Format: date-time
             * @description When the accumulator rolls over to a new period row.
             */
            resets_at: string;
        };
        /**
         * @description The five COST fields — monthly_limit, effective_limit, spend, remaining
         *     and currency — are deliberately NOT in `required`. Both project-scoped
         *     reads (`getProjectBudget`, `getProjectUsage`) REMOVE them for a caller
         *     whose `can_see_amounts` is false, so requiring them would make a
         *     correct redacted response fail its own schema. Everything that is not a
         *     cost figure — the percentage, the threshold, the period — is required,
         *     because redaction never removes those.
         */
        BudgetState: components["schemas"]["BudgetPeriodWindow"] & {
            /**
             * @description Present on the project-scoped reads only. False means the five
             *     cost fields were removed from this payload; percentages, the
             *     warning threshold and the period remain.
             */
            can_see_amounts?: boolean;
            /**
             * @description Always USD. The gateway compares limits against a nano-USD
             *     counter and nothing converts, so the write path rejects any
             *     other value rather than storing a label enforcement ignores.
             * @enum {string}
             */
            currency?: "USD";
            /**
             * @description The ENFORCED ceiling. Null when the scope is unlimited — which
             *     includes a scope that has a monthly_limit but is disabled.
             */
            effective_limit?: number | null;
            /**
             * @description False marks the scope deliberately exempt: it stays unlimited
             *     whatever monthly_limit says. Stored separately from the
             *     enforcement flag so `enabled with no ceiling yet` and
             *     `deliberately exempt` do not read back as the same thing.
             */
            enabled: boolean;
            /**
             * @description Where the enforced limit came from. The reference had a third
             *     value, `default`, for a platform-wide fallback; this platform
             *     has none.
             * @enum {string}
             */
            limit_source: "explicit" | "unlimited";
            /** @description The AUTHORED ceiling in USD. Null means none was set. */
            monthly_limit?: number | null;
            /** @description Null when the scope is unlimited or the limit is zero. */
            percent_used: number | null;
            /** @description Null when the scope is unlimited. Never negative. */
            remaining?: number | null;
            /** @description USD accrued this period. Zero when nothing is recorded. */
            spend?: number;
            /**
             * @description Whether an accumulator row exists for this period at all, which
             *     is what separates "nothing billed yet" from "no usage data".
             */
            spend_available: boolean;
            /**
             * @description True when `percent_used` has reached `warning_pct` — the
             *     soft-alert threshold this scope resolves, which is the
             *     project's own value, else the platform value an operator set
             *     through `PUT /admin/gateway/budget-alerts`, else 80.
             *
             *     The server answers it rather than each client deriving it, so a
             *     warning banner and the percentage beside it cannot disagree,
             *     and so the comparison is made once against the same resolved
             *     threshold the gateway alerts on. False whenever there is
             *     nothing to warn about: an unlimited scope, a disabled one and a
             *     zero ceiling all report a null `percent_used`.
             *
             *     It is not a cost figure and is never redacted: a member who may
             *     not see the amounts still learns the project is close to its
             *     limit.
             */
            warning_active: boolean;
            /** @description Percent-of-limit at which this scope warns. */
            warning_pct: number;
        };
        /** @description Resume a paused turn. NOTE(W2): internal/api/v2/agentexecution/route.go:201 (`currentContinuationBody`); which fields are allowed depends on the `execution_contract` (route.go:484-540). */
        ChatContinueRequest: {
            /** @description `authorize` or `skip` (agent.continue.authorization.v1, single request). */
            authorization_action?: string;
            authorization_request_id?: string;
            /** Format: uuid */
            conversation_uuid: string;
            /** @description The single-pause decision, one the pause frame's `available_actions` offered: `approve`, `reject`, `edit`, `block_with_comment` or `answer`. */
            hitl_action?: string;
            /** @description Per-interrupt decisions for a multi-pause frame, instead of `hitl_action`. */
            hitl_decisions?: {
                [key: string]: unknown;
            }[];
            /** @description True for `agent.continue.hitl.v1` and for a batched authorization answer. */
            hitl_resume?: boolean;
            /** @description The decision's value; its type follows the action. `approve`, `reject`: absent. `edit`: a non-empty STRING — the edited value (for a tool-call pause, the edited arguments as JSON text). `block_with_comment`: a non-empty string, the comment. `answer` (an ask_user pause): an OBJECT keyed by question id (each value the chosen option(s) or free text), or a plain string. The same rule applies to each `hitl_decisions[].value`. */
            hitl_value?: unknown;
            ignored_mcp_servers?: string[];
            mcp_tokens?: {
                [key: string]: unknown;
            };
            /**
             * Format: uuid
             * @description The paused answer's message id.
             */
            message_id: string;
            /** Format: int64 */
            project_id: number;
            thread_id?: string;
            user_declined_mcp_servers?: string[];
            user_input?: string;
        };
        /** @description A refused send, regenerate or continue. NOTE(W2): internal/api/v2/agentexecution/route.go:558 (`writeStartError`) and :601 (`writeUnsupported`). `error` is a machine code for 409 and 422 (`agent_regeneration_pending`, `agent_hitl_already_resolved`, `agent_authorization_already_resolved`, `agent_output_limit_already_resolved`, `unsupported_agent_execution`) and a message otherwise; the idempotency conflict is a 409 whose `error` is the message "Agent execution request conflicts with an existing turn". */
        ChatExecutionError: {
            error: string;
            message?: string;
            retryable?: boolean;
        };
        /** @description The admitted (or replayed) turn. NOTE(W2): internal/api/v2/agentexecution/route.go:330-337, :423-430 and :550-557 (one shape for send, regenerate and continue). */
        ChatExecutionStarted: {
            command_id: string;
            /** @description True when this request admitted the turn; false when it replayed an earlier admission with the same idempotency key (the same `execution_id` and `events_url` come back). */
            created: boolean;
            /** @description The turn's event stream, ALWAYS a same-origin absolute path (`/api/v2/executions/{project_id}/{execution_id}/events`), never a full URL. Resolve it against the deployment origin. */
            events_url: string;
            execution_id: string;
            /** @description The UUID of the assistant message group the turn writes (its `uid` in the message list). */
            response_message_id: string;
            /** @description Equal to `execution_id` (legacy name). */
            task_id: string;
        };
        /** @description NOTE(W2): internal/api/v2/agentexecution/route.go:183 (`currentRegenerationBody`). `message_id` and `stream_id` must both equal the path's message id. */
        ChatRegenerateRequest: {
            /** Format: uuid */
            conversation_uuid: string;
            /**
             * Format: uuid
             * @description The answer being regenerated; equals the path's message id.
             */
            message_id: string;
            /** Format: int64 */
            participant_id?: number;
            payload: {
                llm_settings?: {
                    [key: string]: unknown;
                };
                mcp_tokens?: {
                    [key: string]: unknown;
                };
                user_input: string;
            };
            /** Format: int64 */
            project_id: number;
            /**
             * Format: uuid
             * @description The question being re-answered (lowercase canonical UUID).
             */
            question_id: string;
            /**
             * Format: uuid
             * @description Client generated, lowercase canonical UUID: the idempotency key of this regeneration. A retry with the same id replays it.
             */
            regeneration_id: string;
            /**
             * Format: uuid
             * @description Equals `message_id`.
             */
            stream_id: string;
            /** @description Absent or empty for a plain retry. One `text_message` item to regenerate from an EDITED question. */
            updated_items?: {
                content: string;
                /** @description Must be `text_message`. */
                item_type: string;
                uuid?: string;
            }[];
        };
        /** @description The body of a chat send. NOTE(W2): internal/api/v2/agentexecution/route.go:143 (`currentApplicationStartBody`). The body must be ONE JSON value of at most 512 KiB. */
        ChatSendRequest: {
            /**
             * Format: uuid
             * @description Must equal the path's conversation id.
             */
            conversation_uuid: string;
            /** Format: uuid */
            interaction_uuid?: string;
            /** @description `@everyone`: every project member except the sender is notified. */
            is_mentioning_everyone?: boolean;
            /** @description Required for `agent.execute.adhoc.v1` (the model and its settings); must be absent for an application turn. */
            llm_settings?: {
                [key: string]: unknown;
            };
            mcp_tokens?: {
                [key: string]: unknown;
            };
            /**
             * Format: int64
             * @description The participant that answers. Required (> 0) for `agent.execute.application.v1`; 0 or absent for a model chat (`agent.execute.adhoc.v1`).
             */
            participant_id?: number;
            payload: {
                attachments?: components["schemas"]["ChatStartAttachment"][];
                user_input: string;
            };
            /**
             * Format: int64
             * @description Must equal the path's project id.
             */
            project_id: number;
            /**
             * Format: uuid
             * @description REQUIRED, client generated, a LOWERCASE canonical UUID (`xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`; an upper-case spelling is refused). It is the admission idempotency key of the turn: generate it once per user message, persist it with the unsent message, and send the same value on every retry.
             */
            question_id: string;
            /** @description USER ids this message mentions (at most 64; `entity_meta.id` of a user participant, never a participant row id). `userIds` is accepted as an alias. Each project member named, except the sender, gets one `chat_user_mentioned` notification once the turn is admitted — on both execution contracts (an ad-hoc model chat notifies since client contract 1.1). An id that is not a project member is dropped silently; a replay of the same `question_id` notifies nobody again. */
            user_ids?: number[];
        };
        ChatStartAttachment: {
            /** @description Exactly the `filepath` the attachment upload returned. */
            filepath: string;
            /** @description Display name; ignored by the server. */
            name?: string;
        };
        /** @description The deployment's chat attachment limits (client contract 1.1), so a client can refuse a file before uploading it; the server enforces every one of them again. Byte values are bytes. A project's storage policy may set a different per-file cap, which the upload's 400 names. NOTE(W2): internal/api/v2/discovery/document.go:60 (`AttachmentPolicy`), filled by internal/api/v2/conversations/attachment_limits.go:47 (`CurrentAttachmentLimits`). */
        ClientAttachmentPolicy: {
            /** @description Lower-case, leading-dot extensions the model can read. */
            accepted_extensions: string[];
            /**
             * Format: int64
             * @description The largest single upload request; send a larger file in chunks of at most this size.
             */
            chunk_bytes: number;
            /** @description True when the server downscales a larger image in one of `inline_image_formats` to fit `inline_image_max_bytes`, so a client need not. */
            inline_image_downscale: boolean;
            /** @description Extensions handed to the model as images. */
            inline_image_formats: string[];
            /**
             * Format: int64
             * @description The largest image (raw bytes) handed to the model as an image.
             */
            inline_image_max_bytes: number;
            /**
             * Format: int64
             * @description The largest document whose text is extracted for the model; a larger one is stored but not read.
             */
            max_extract_bytes: number;
            /**
             * Format: int64
             * @description One non-image file (ARTIFACT_ATTACHMENT_MAX_FILE_MB).
             */
            max_file_bytes: number;
            /** @description Files one message carries in the composer. */
            max_files: number;
            /**
             * Format: int64
             * @description One image (ARTIFACT_ATTACHMENT_MAX_IMAGE_MB).
             */
            max_image_bytes: number;
            /**
             * Format: int64
             * @description One upload, all chunks of one file (ARTIFACT_ATTACHMENT_MAX_TOTAL_MB).
             */
            max_total_bytes: number;
        };
        /** @description The public subset of the native client policy (ADR-0025 decision 5); the full policy travels with every token response. NOTE(W2): internal/api/v2/discovery/document.go:65 (`PublicPolicy`). */
        ClientPublicPolicy: {
            /** @description Client contract 1.3. Dictation may use a recogniser that sends audio off the device (the platform's server recogniser or the workspace's transcription model). False means on-device recognition only. Default false. */
            allow_cloud_stt: boolean;
            /** @description Client contract 1.3. A notification may offer actions (mark read, open an approval). A decision always needs the app unlocked. Default true. */
            allow_notification_actions: boolean;
            /** @description Client contract 1.3. The system share sheet may send text, links, files and photos into the app. Default true. */
            allow_share_in: boolean;
            /** @description Client contract 1.3. Copy, share and export of messages and transcripts out of the app are allowed. Default true. */
            allow_share_out: boolean;
            /** @description Client contract 1.3. Widgets, home-screen quick actions and other surfaces outside the app may show conversation and agent titles. False means counts only. Default false. */
            allow_system_surfaces: boolean;
            /** @description Deployment-wide minimum client version; empty means none. The document's per-client `min_client_version` entry, when present, is never lower than this. */
            min_client_version: string;
            /** @description Client contract 1.3. What a notification shows on the lock screen: `none` (a generic text only, the default) or `title` (the conversation or item title). Message content is never shown. Treat an unknown value as `none`. */
            notification_preview: string;
            /** @description Offline storage is allowed. */
            offline_enabled: boolean;
            /** @description The client must refuse to run without a device passcode or biometric lock. */
            require_device_lock: boolean;
        };
        /** @description The 426 answer to a native client whose `X-Client-Version` is below the minimum (ADR-0025 decision 5). Only callers authenticated by a native access token, and the native token endpoint, are judged. NOTE(W2): internal/api/middleware/client_version.go:176 (`WriteClientUpgradeRequired`). */
        ClientUpgradeRequiredError: {
            /** @description Always client_upgrade_required. */
            error: string;
            error_description?: string;
            /** @description The minimum this client must update to. */
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
            /** @description `reviewed` only, and llm_model only. The optional one-line text an admin writes on the model (at most 40 characters). Pickers show it under the model name. It is display text and never reaches the provider. Absent when the model has none. `compat` carries the same value inside `data`. */
            description?: string | null;
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
            /** @description `reviewed` only. True when `default_model_name` is the default an admin configured for the project or the platform. False when no configured default is in the catalogue and `default_model_name` is only the first item. Omitted when false. */
            default_model_configured?: boolean;
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
        /** @description A chunk of a chunked upload was stored; the file is assembled when the last chunk arrives, and that request answers 201. NOTE(W2): internal/api/v2/conversations/attachments.go:409. */
        ConversationAttachmentChunkReceived: {
            chunk_index: number;
            file_id: string;
            message: string;
            /** @description Always `chunk_received`. */
            status: string;
            total_chunks: number;
        };
        /** @description One stored file. Send `filepath` back verbatim in the chat send's `payload.attachments`. NOTE(W2): internal/api/v2/conversations/attachments.go:217 (`attachmentCreated`). */
        ConversationAttachmentUploaded: {
            /** Format: int64 */
            file_size: number;
            /** @description `/{bucket}/{conversation_uuid}/{file name}`. */
            filepath: string;
        };
        /** @description NOTE(W2): read key by key from a free JSON body by internal/api/v2/conversations/handler.go:653 (`Create`). Unknown keys are ignored. */
        ConversationCreateRequest: {
            instructions?: string;
            /** @description Defaults to private when absent. A non-boolean value is a 400. */
            is_private?: boolean;
            meta?: {
                [key: string]: unknown;
            };
            name?: string;
            /** @description Participants to add at creation. Each needs `entity_name` and `entity_meta`; `entity_meta.project_id` defaults to this project. */
            participants?: components["schemas"]["ConversationParticipant"][];
            /** @description The conversation's origin (`chat` by default; `deepwiki` and similar for hidden ones). */
            source?: string;
        };
        /** @description One conversation with its participants, and with its newest message groups when `messages_limit` is set. NOTE(W2): internal/api/v2/conversations/handler.go:581 (`Get`); the map is built at :604-628. */
        ConversationDetail: {
            /** Format: date-time */
            created_at: string;
            created_by: string;
            description?: string;
            editor_test_runs?: components["schemas"]["EditorTestRunsPage"];
            /** @description Null when the conversation sits outside every folder. */
            folder_id?: string | null;
            id: string;
            instructions?: string;
            /** @description Always present; a row that cannot say reads as private. */
            is_private: boolean;
            message_count: number;
            /** @description Present only when `messages_limit` > 0. The legacy message-group projection (items and their details). Prefer the message list (listConversationMessages) for a transcript: it is the read that supports `changes_since`. */
            message_groups?: {
                [key: string]: unknown;
            }[];
            meta: {
                editor_test?: components["schemas"]["EditorTestContext"];
                is_hidden?: boolean;
            } & {
                [key: string]: unknown;
            };
            name: string;
            participants: components["schemas"]["ConversationParticipant"][];
            project_id: string;
            source?: string;
            /** Format: date-time */
            updated_at: string;
            /** Format: uuid */
            uuid?: string;
        };
        /** @description The `format=json` body. The Markdown document is rendered from this same model, so the two formats cannot describe different conversations. */
        ConversationExport: {
            /** Format: date-time */
            created_at: string;
            description?: string;
            /**
             * Format: date-time
             * @description When this document was produced, in UTC.
             */
            exported_at: string;
            id: string;
            /** @description The number of messages in `messages`, which is the whole transcript. */
            message_count: number;
            messages: components["schemas"]["ConversationExportMessage"][];
            name: string;
            project_id: string;
            /** Format: date-time */
            updated_at: string;
            uuid?: string;
        };
        ConversationExportMessage: {
            /** @description The display name of the participant that wrote the message, when the participant list resolves one. Absent for a message whose author cannot be named; the Markdown document then falls back to the role. */
            author?: string;
            content: string;
            content_type?: string;
            /** Format: date-time */
            created_at: string;
            id: string;
            metadata?: {
                [key: string]: unknown;
            };
            /** @description `user` or `assistant` as the transcript stored it. Passed through rather than normalised, so an export records what was written. */
            role: string;
            /** @description The message group's UUID, when it has one. */
            uid?: string;
        };
        /** @description The legacy page carries `total` and `rows` only. With `changes_since` (ADR-0025 WP6) the same two keys keep their meaning — the rows are the legacy row shape, oldest change first, and `total` is the size of the caller's whole filtered list now — and `tombstones`, `next_cursor` and `has_more` are added. */
        ConversationListing: {
            /** @description Delta mode only. True when this page was cut short by `limit`; call again with `next_cursor` straight away. */
            has_more?: boolean;
            /** @description Delta mode only. The opaque cursor for the next call. It never moves past the database clock minus a 5 s settle window, so rows changed within that window come back again; upsert by id. */
            next_cursor?: string;
            rows: components["schemas"]["ConversationSummary"][];
            /** @description Delta mode only. Conversations deleted (`deleted`) or gone from this list for this caller (`access_lost`: made private, the caller removed as a participant, or no longer matching the list filter) since the cursor. A private conversation's deletion is told only to its author, its former user participants and project admins. */
            tombstones?: components["schemas"]["SyncTombstone"][];
            total: number;
        };
        /** @description One message group of a transcript. NOTE(W2): internal/api/v2/conversations/handler.go:74 (`Message`). Delta rows (`changes_since`) use the same projection, byte for byte. */
        ConversationMessage: {
            author_participant_id?: number;
            content: string;
            content_type?: string;
            conversation_id: string;
            /** Format: date-time */
            created_at: string;
            /** @description The message group's numeric id, as a string. Tombstones carry it as an integer. */
            id: string;
            /** @description Present (true) only while the turn is in flight. */
            is_streaming?: boolean;
            /** @description Omitted, not empty, for a group with no attachments. */
            message_items?: components["schemas"]["AttachmentMessageItem"][];
            metadata?: {
                [key: string]: unknown;
            };
            reply_to_id?: number;
            /** @description `user` or `assistant`. */
            role: string;
            sent_to_id?: number;
            /** @description The in-flight turn's execution id; present only with `is_streaming`. */
            task_id?: string;
            /** @description The message group's UUID. For an assistant answer this is the `response_message_id` the send returned. */
            uid: string;
            /**
             * Format: date-time
             * @description When the group was last rewritten (an answer finalised or regenerated). Omitted when never.
             */
            updated_at?: string;
        };
        /** @description The transcript page. Legacy mode carries `items`, `total`, `page`, `page_size` and `total_pages` (NOTE(W2): internal/api/v2/conversations/handler.go:148). With `changes_since` (ADR-0025 WP6) it carries `items` (changed groups, oldest change first), `total` (groups in the conversation now), `tombstones`, `next_cursor` and `has_more` (internal/api/v2/conversations/changes.go, `MessageChanges`). */
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
        /** @description One participant of a conversation (a user, an agent or pipeline, a toolkit, a model). NOTE(W2): internal/api/v2/conversations/handler.go:240 (`Participant`). */
        ConversationParticipant: {
            /** @description The stable keys are typed below; every other key is the kind's own. For a `user` participant `id` is the USER id — the value a chat send's `user_ids` mention list carries — never the participant row id. */
            entity_meta?: ({
                /** @description The entity id (user, application, pipeline or toolkit). Absent for `llm` and `dummy`. */
                id?: number;
                /** @description An `llm` participant's model. */
                model_name?: string;
                /** @description The project that owns an application, pipeline or toolkit. */
                project_id?: number;
            } & {
                [key: string]: unknown;
            }) | null;
            /** @description `user`, `application`, `toolkit`, `llm` and similar; read it, do not enumerate it. */
            entity_name: string;
            entity_settings?: ({
                /** @description The application or pipeline version a chat send to this participant runs. */
                version_id?: number;
            } & {
                [key: string]: unknown;
            }) | null;
            id: number;
            meta?: ({
                /** @description Every other kind's display name. */
                name?: string;
                /** @description A `user` participant's display name. */
                user_name?: string;
            } & {
                [key: string]: unknown;
            }) | null;
        };
        /** @description One participant to add (see `addConversationParticipants` for the keys each kind needs). NOTE(W2): internal/api/v2/conversations/participant_settings.go:131 (`Participant.Validate`). */
        ConversationParticipantCreate: {
            entity_meta: {
                /** @description The user, application, pipeline or toolkit id. */
                id?: number;
                /** @description An `llm` participant's model. */
                model_name?: string;
                /** @description The display name of an application, pipeline or toolkit. */
                name?: string;
                /** @description The owning project of an application, pipeline or toolkit; defaults to the path project. */
                project_id?: number;
                /** @description A user's display name. */
                user_name?: string;
            } & {
                [key: string]: unknown;
            };
            /** @description `user`, `application`, `pipeline`, `toolkit`, `llm` or `dummy`. */
            entity_name: string;
            entity_settings?: {
                /** @description The application or pipeline version to run. */
                version_id?: number;
            } & {
                [key: string]: unknown;
            };
        };
        /** @description NOTE(W2): opaque jsonb array round-trip — see items marker. One caveat to the "never inspects" claim: runPublishValidation decodes the column as []string (eliteacore/handler.go:1028-1029), so non-string elements are silently read as an empty list there (spurious "no conversation starters" warning); the store/return round-trip itself is untyped. */
        ConversationStarters: unknown[];
        /** @description One row of listConversations — the run-history panel's (issue #868) light projection of a conversation. `duration` is a placeholder: internal/api/v2/conversations/handler.go's List always answers `-1` for it (no per-conversation duration is computed server-side yet); a caller wanting a displayed duration derives one from created_at/updated_at instead of trusting this field. */
        ConversationSummary: {
            created_at: string;
            /** @description Always -1 today; see the schema description. */
            duration: number;
            id: number;
            /** @description Client contract 1.4. The conversation is pinned in its project: the project holds a conversation pin for it (pinEntity with entity_type `conversation`, the pin the web app's "Pin on top" sets). A pin is shared by the project, not kept per member, and the row never says who pinned it. A pin or unpin re-delivers the row in the `changes_since` delta; it does not change `updated_at`. A server before 1.4 omits the field, which means not pinned. */
            is_pinned: boolean;
            message_groups_count: number;
            /** @description Carries `single_participant` (the entity this conversation was filtered by, when entity_name/entity_meta_id were sent) among other conversation-level flags (`is_hidden`, etc). */
            meta?: {
                [key: string]: unknown;
            } | null;
            name: string;
            updated_at: string;
        };
        /** @description A conversation as the create route answers it (201). `id` is the numeric id AS A STRING; `uuid` is the id the chat send path takes. NOTE(W2): internal/api/v2/conversations/handler.go:32 (`Conversation`), written at :740 (201). */
        CreatedConversation: {
            /** Format: date-time */
            created_at: string;
            created_by: string;
            description?: string;
            folder_id?: string | null;
            id: string;
            instructions?: string;
            is_private?: boolean;
            message_count: number;
            meta?: {
                [key: string]: unknown;
            };
            name: string;
            participants?: components["schemas"]["ConversationParticipant"][];
            project_id: string;
            source?: string;
            /** Format: date-time */
            updated_at: string;
            /** Format: uuid */
            uuid?: string;
        };
        /** @description The ADR-0025 `device_revoked` answer: FLAT, unlike the nested error envelope, so a native client tells "wipe local data" (`error` is the string device_revoked) from "refresh once" (`error` is an object). Sent only for native credentials, with `WWW-Authenticate: Bearer error="invalid_token", error_description="device_revoked"`. NOTE(W2): internal/api/middleware/auth.go:453 (`DeviceRevokedBody`). */
        DeviceRevokedError: {
            /** @description Always device_revoked. */
            error: string;
            error_description?: string;
        };
        /** @description The public discovery document served at `/.well-known/elitea-client` (ADR-0025 decision 1). The key set is a contract: adding a key is additive, renaming or removing one bumps `client_contract`. NOTE(W2): marshalled from internal/api/v2/discovery/document.go:36 (`Document`). */
        DiscoveryDocument: {
            attachments: components["schemas"]["ClientAttachmentPolicy"];
            /** @description Absolute, content-addressed URL of the brand pack JSON (`.../api/v2/branding/pack.json?v=<etag>`). */
            brand_pack_url: string;
            /**
             * @description The client contract version this server speaks (ADR-0025 decision 6).
             * @example 1.0
             */
            client_contract: string;
            client_policy: components["schemas"]["ClientPublicPolicy"];
            /**
             * @description Who operates the deployment (ELITEA_DEPLOYMENT_KIND).
             * @enum {string}
             */
            deployment_kind: "saas" | "self_hosted";
            /** @description The product name from the resolved brand pack. */
            display_name: string;
            /** @description Registered, enabled client id → the effective minimum version this deployment serves that client: the higher of `client_policy.min_client_version` and the client's own minimum. A client with no effective minimum is absent, so a client reads its own entry and falls back to `client_policy.min_client_version`. Always an object. */
            min_client_version: {
                [key: string]: string;
            };
            /** @description The native authorization endpoints, or null while no native client is registered on this deployment. */
            native_auth: components["schemas"]["NativeAuthEndpoints"] | null;
            /**
             * @description The server release as `major.minor` only (the full build string is admin-only), or `dev` for a build without a release version.
             * @example 1.62
             */
            server_version: string;
        };
        /** @description Immutable Test identity. Main derives actor and project from authenticated authority. */
        EditorTestContext: {
            actor_id: string;
            application_id: string;
            application_version_id: string;
            project_id: string;
            /** @enum {integer} */
            revision: 1;
        };
        EditorTestInputReference: {
            bundle_id: string;
            content_digest: string;
            entry_id: string;
            immutable_version: string;
        };
        /** @description Existing admitted run. Reading or observing this row creates no work. */
        EditorTestRun: {
            /** Format: date-time */
            admitted_at: string;
            can_control: boolean;
            desired_state: string;
            events_url?: string;
            execution_generation: string;
            execution_id: string;
            input_reference: components["schemas"]["EditorTestInputReference"];
            /** @enum {string} */
            phase: "RUNNING" | "PAUSED" | "TERMINAL";
            /** Format: uuid */
            question_id: string;
            /**
             * Format: int64
             * @description Original response group for this execution and generation. Never substitute a newer response.
             */
            response_message_group_id?: number;
            /** Format: uuid */
            response_message_id: string;
            /** Format: date-time */
            settled_at?: string | null;
            state: string;
            /** @description Visible persisted trace exists and the original response still matches this execution and generation. Omission on older Main versions means unavailable. */
            trace_available?: boolean;
        };
        EditorTestRunsPage: {
            has_more: boolean;
            limit: number;
            offset: number;
            rows: components["schemas"]["EditorTestRun"][];
        };
        /** @description NOTE(W2): wire truth from pkg/apierr/apierr.go:47-59 (apierr.Write encodes `{"error": message}`) and the handlers' inline `map[string]any{"error": ...}` writes. No operation documented in this spec emits the pylon `ok` field via THIS schema; the five social/handler.go 500 sites (:268, 301, 321, 341, 419) use the named `SocialActionErrorResponse` schema instead (see there), and five more UNDOCUMENTED sites remain on toolkits/handler.go (:286, 312, 324, 350, 362). */
        ErrorResponse: {
            /** @description Human-readable error message */
            error: string;
        };
        /** @description The 400 answer to a native client whose `X-Client-Version` does not parse (ADR-0025 decision 5). NOTE(W2): internal/api/middleware/client_version.go:190 (`WriteInvalidClientVersion`). */
        InvalidClientVersionError: {
            /** @description Always invalid_client_version. */
            error: string;
            error_description: string;
        };
        /** @description The 400 answer of listPublicApplications. A query parameter outside its allowlist or range answers `error: invalid_query_parameter` with `param` naming it and `msg` saying what it must be; the version gate answers `error: invalid_client_version` with no `param`. NOTE(W2): internal/api/v2/eliteacore/public_applications.go:280 (`writePublicApplicationsQueryError`). */
        InvalidQueryParameterError: {
            error: string;
            error_description: string;
            msg?: string;
            param?: string;
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
        /** @description The user's default context-management settings, stored in centry.social_users.default_context_management (jsonb) and edited by Settings > Memory. Every property is optional: absent means "the user has no opinion", which resolves to the platform default, and is a different answer from an explicit false or zero. Ranges are enforced on write (internal/domain/contextsettings); an out-of-range value is refused with {"error", "field"}. `enable_context_editing` is not in pylon's ContextManagementModel but IS read out of the same dict by set_context_strategy, and the Memory page has always sent it. */
        MemoryContextManagement: {
            /**
             * @description Balanced caps the combined context window at 272000 tokens or the model window, whichever is smaller. Full uses the model window. Both reserve model output and a safety margin before calculating usable input. Absent legacy selections resolve to balanced.
             * @enum {string}
             */
            budget_mode?: "balanced" | "full";
            enable_context_editing?: boolean;
            enabled?: boolean;
            /**
             * @deprecated
             * @description Legacy preference. New execution budgets use budget_mode.
             */
            max_context_tokens?: number;
            preserve_recent_messages?: number;
        };
        /** @description One persistent, cross-conversation personal memory (#870). internal/api/v2/memories/handler.go. Scoped to (project, the authenticated caller) — never readable or writable cross-user. `enabled=false` keeps the entry without letting it reach a chat turn's recall (internal/application/agentexecution's CurrentMemoryRecallResolver). NOT the same feature as the `memory` TOOLKIT type (integrations/toolkits/memory_toolkit) — that is an agent-scoped key/value store the LLM calls as a tool mid-turn; this is read passively, before a turn starts. */
        MemoryEntry: {
            /** @description The remembered fact's text. Refused past 4000 characters (memories.MaxContentBytes). */
            content: string;
            /** Format: date-time */
            created_at: string;
            enabled: boolean;
            id: string;
            project_id: string;
            /** @description The conversation this memory was captured from, when it was saved via the "Remember this" message action (chat_conversations.uuid). Absent when typed directly into the settings panel. Informational only — the source conversation may since have been deleted (tenant/0137_personal_memory_entries.sql's own header explains why this deliberately holds no foreign key). */
            source_conversation_id?: string | null;
            /** @description Free-form labels for the settings panel's own search/filter. Never more than 16 (memories.MaxTags). */
            tags: string[];
            /** Format: date-time */
            updated_at: string;
        };
        MemoryEntryList: {
            items: components["schemas"]["MemoryEntry"][];
            total: number;
        };
        /** @description The user's default summarization settings, stored in centry.social_users.default_summarization (jsonb) and edited by Settings > Memory. pylon's SummarizationModel, field for field. Three of these flatten into a conversation's `context_strategy.summary_llm_settings` when one is resolved: summary_model_name -> model_name, summary_model_project_id -> model_project_id, target_summary_tokens -> max_tokens. */
        MemorySummarization: {
            enable_summarization?: boolean;
            min_messages_for_summary?: number;
            summary_instructions?: string;
            summary_model_name?: string;
            summary_model_project_id?: number;
            summary_trigger_ratio?: number;
            target_summary_tokens?: number;
        };
        /** @description One user's like/dislike + optional comment on a message (#880). `rating` is never `0` — the stored row's own CHECK constraint refuses it (tenant/0135_chat_message_feedback.sql) — only its ABSENCE (no `mine` on the summary) means "not rated". */
        MessageFeedback: {
            comment?: string;
            /**
             * @description -1 = dislike, 1 = like.
             * @enum {integer}
             */
            rating: -1 | 1;
        };
        MessageFeedbackRequest: {
            /** @description Optional. Refused with a 400 past 2000 characters (messageFeedbackCommentMaxLen). */
            comment?: string;
            /**
             * @description -1 = dislike, 1 = like. Any other value is a 400.
             * @enum {integer}
             */
            rating: -1 | 1;
        };
        /** @description What every message-feedback endpoint answers: the aggregate like/dislike counts across every user who has rated this message, plus the CALLING user's own feedback. The client's hover tooltip reads the counts; the thumbs buttons' highlighted state reads `mine`. */
        MessageFeedbackSummary: {
            dislikes: number;
            likes: number;
            mine?: components["schemas"]["MessageFeedback"] | null;
        };
        MessageTraceListing: {
            rows: components["schemas"]["MessageTraceStep"][];
            /** @description Null unless include_total=true was requested. */
            total: number | null;
        };
        /** @description The light projection of a chat_message_trace_step row: what a resting chip draws, plus what orders it. */
        MessageTraceStep: {
            /** @description For a `tool_call` step the keys a client renders are typed below (client contract 1.1); every other key is display-only and may change. */
            attrs?: ({
                metadata?: {
                    display_name?: string;
                    /** @description The tool's own name when `tool_name` is a prefixed one. */
                    original_name?: string;
                    /** @description The sub-agent that ran the tool. */
                    parent_agent_name?: string;
                    /** @description The toolkit the tool belongs to. */
                    toolkit_name?: string;
                    toolkit_type?: string;
                } & {
                    [key: string]: unknown;
                };
                /** @description Contract 1.2. Present on a tool call that paused for the user: the `interrupt_id` of the approval card it waited on and the `guardrail_type` (`sensitive_tool`, `clarifying_question`). */
                pause?: {
                    guardrail_type?: string;
                    interrupt_id?: string;
                };
                response_metadata?: {
                    [key: string]: unknown;
                };
                tool_meta?: {
                    display_name?: string;
                    name?: string;
                } & {
                    [key: string]: unknown;
                };
                /** @description Present on a tool call whose output was chunked: how many of its `total` chunks were `received`, and whether the reassembly is `complete` (its SHA-256 matched). */
                tool_output_chunks?: {
                    complete?: boolean;
                    received?: number;
                    /** @description A NUL byte was stripped from at least one chunk. */
                    sanitized?: boolean;
                    tool_output_sha256?: string;
                    total?: number;
                };
            } & {
                [key: string]: unknown;
            }) | null;
            /** @description For a tool call: `stop`, `error`, or (contract 1.2) a pause reason — `awaiting_approval`, `awaiting_input`, `interrupted`. A client treats an unknown value by `is_error`. */
            finish_reason?: string | null;
            /** Format: date-time */
            finished_at?: string | null;
            /** Format: int64 */
            id: number;
            /** @description True for a tool call that failed. A call that paused for the user (contract 1.2, `finish_reason` `awaiting_approval`, `awaiting_input` or `interrupted`) is not an error. */
            is_error: boolean;
            /** @description `tool_call` or `thinking_step`. */
            kind: string;
            /** Format: int64 */
            message_group_id: number;
            model_name?: string | null;
            parent_agent_call_id?: string | null;
            parent_agent_name?: string | null;
            /** Format: date-time */
            started_at?: string | null;
            step_type?: string | null;
            tool_name?: string | null;
        };
        MessageTraceStepDetail: components["schemas"]["MessageTraceStep"] & {
            text?: string | null;
            thinking?: string | null;
            tool_inputs?: unknown;
            tool_output?: string | null;
        };
        /** @description Endpoints of the native authorization server (ADR-0025 decision 3). Every URL is absolute. NOTE(W2): internal/api/v2/discovery/document.go:54 (`NativeAuth`). */
        NativeAuthEndpoints: {
            authorization_endpoint: string;
            code_challenge_methods_supported: string[];
            /** @description The deployment origin; a client compares an authorization response's `iss` (RFC 9207) against it. */
            issuer: string;
            revocation_endpoint: string;
            token_endpoint: string;
        };
        /** @description The server-driven native client policy (ADR-0025 decision 5), the `native_client_policy` admin Configuration section. The device-side fields are enforced by the client; the server refuses versions below `min_client_version` with 426. Adding a field is additive; renaming or removing one bumps `client_contract`. NOTE(W2): internal/platformconfig/nativeclientpolicy.go:43 (`NativeClientPolicy`). */
        NativeClientPolicy: {
            /** @description Client contract 1.3. Dictation may use a recogniser that sends audio off the device (the platform's server recogniser or the workspace's transcription model). False means on-device recognition only. Default false. */
            allow_cloud_stt: boolean;
            /** @description Client contract 1.3. A notification may offer actions (mark read, open an approval). A decision always needs the app unlocked. Default true. */
            allow_notification_actions: boolean;
            /** @description When false, block screenshots and screen recording where the platform allows it. */
            allow_screenshots: boolean;
            /** @description Client contract 1.3. The system share sheet may send text, links, files and photos into the app. Default true. */
            allow_share_in: boolean;
            /** @description Client contract 1.3. Copy, share and export of messages and transcripts out of the app are allowed. Default true. */
            allow_share_out: boolean;
            /** @description Client contract 1.3. Widgets, home-screen quick actions and other surfaces outside the app may show conversation and agent titles. False means counts only. Default false. */
            allow_system_surfaces: boolean;
            /**
             * Format: int64
             * @description Lock the app after this many seconds in the background; 0 never locks. 0 to 86400.
             */
            idle_lock_seconds: number;
            /** @description The lowest version this deployment serves this client; empty means none. MAJOR.MINOR.PATCH with an optional -prerelease. */
            min_client_version: string;
            /** @description Client contract 1.3. What a notification shows on the lock screen: `none` (a generic text only, the default) or `title` (the conversation or item title). Message content is never shown. Treat an unknown value as `none`. */
            notification_preview: string;
            /** @description Whether attachments may be stored offline, or only messages. */
            offline_attachments: boolean;
            /**
             * Format: int64
             * @description The most the app may store for offline use, in megabytes.
             */
            offline_max_mb: number;
            /**
             * Format: int64
             * @description How long data may stay on the device without reaching the server; 0 disables offline storage. 0 to 90.
             */
            offline_retention_days: number;
            /** @description Refuse to run on a device without a passcode or biometric lock. */
            require_device_lock: boolean;
        };
        /** @description One native device session (refresh-token family). NOTE(W2): internal/api/nativeauth/devices.go:36 (`UserDevice`). */
        NativeDevice: {
            client_id: string;
            client_name: string;
            client_version: string;
            /** Format: date-time */
            created_at: string;
            /** @description The device whose token authenticated this request. */
            current: boolean;
            /** @description The name the app claimed at sign-in. */
            device_name: string;
            /** Format: uuid */
            id: string;
            /** Format: date-time */
            last_seen_at: string;
            /** @description ios, android, macos, windows, linux or other. */
            platform: string;
            /** @description signed_out, user, admin, refresh_reuse, code_replay, user_deactivated, client_disabled, client_removed or expired. */
            revoke_reason: string | null;
            /** Format: date-time */
            revoked_at: string | null;
        };
        /** @description NOTE(W2) internal/api/nativeauth/devices.go:108. */
        NativeDeviceList: {
            devices: components["schemas"]["NativeDevice"][];
        };
        /** @description RFC 6749 section 5.2 error of the native authorization endpoints. NOTE(W2): internal/api/nativeauth/token.go:271 (`writeOAuthError`). */
        NativeOAuthError: {
            /** @description invalid_request, invalid_client, invalid_grant, unsupported_grant_type, temporarily_unavailable or not_found. */
            error: string;
            error_description?: string;
        };
        /** @description The RFC 7009 revocation form. NOTE(W2) internal/api/nativeauth/token.go:277. */
        NativeRevokeRequest: {
            client_id: string;
            token: string;
            /** @description Ignored; the token's prefix decides. */
            token_type_hint?: string;
        };
        /** @description The token request form. NOTE(W2): internal/api/nativeauth/token.go:40 (`tokenParameters`). */
        NativeTokenRequest: {
            client_id: string;
            /** @description The client's version, recorded on the device (refresh_token grant). */
            client_version?: string;
            /** @description The authorization code (authorization_code grant). */
            code?: string;
            /** @description The PKCE verifier, 43 to 128 unreserved characters (authorization_code grant). */
            code_verifier?: string;
            /** @description authorization_code or refresh_token. */
            grant_type: string;
            /** @description The redirect URI the code was issued to (authorization_code grant). */
            redirect_uri?: string;
            /** @description The current refresh token (refresh_token grant). */
            refresh_token?: string;
        };
        /** @description A native token pair. Both tokens are opaque. NOTE(W2): internal/api/nativeauth/token.go:242 (`writeGrant`). */
        NativeTokenResponse: {
            /** @description Opaque bearer token, prefix elnat_. */
            access_token: string;
            /** @description The full native client policy, with `min_client_version` resolved for this client. Present on every token response; absent only if the policy could not be read since this server started, in which case the client keeps the policy it last received. */
            client_policy?: components["schemas"]["NativeClientPolicy"];
            /** @description The device session (refresh-token family) id. */
            device_id: string;
            /**
             * Format: int64
             * @description Seconds until the access token expires.
             */
            expires_in: number;
            /** @description Opaque refresh token, prefix elnrt_; rotated on every use. */
            refresh_token: string;
            /**
             * Format: int64
             * @description Seconds of idle time before the device must sign in again.
             */
            refresh_token_expires_in: number;
            /** @description Always Bearer. */
            token_type: string;
        };
        /** @description NOTE(W2): internal/api/v2/notifications/api.go:153 (`currentNotificationResponse`). In a list `created_at`/`updated_at` are RFC 3339 to the second; a single read keeps microseconds without a zone. */
        Notification: {
            created_at: string;
            event_type: string;
            id: number;
            is_seen: boolean;
            /** @description Per `event_type`. For `chat_user_mentioned` (client contract 1.1) the server writes `conversation_id` (the conversation UUID), `message_id` (the mentioning message's `question_id`), `project_id`, `sender_user_id` and `sender` (internal/infra/db/repos/chat_mention_notification.go). A row a pre-Go deployment wrote carries the NUMERIC conversation id, `initiator_name` and no `sender`; getConversation resolves either id form, so open the conversation with whatever `conversation_id` holds. */
            meta: {
                /** @description `chat_user_mentioned`: who mentioned the recipient. `name` is empty when it could not be resolved. */
                sender?: {
                    /** Format: int64 */
                    id: number;
                    name: string;
                };
            } & {
                [key: string]: unknown;
            };
            project_id: number;
            updated_at: string | null;
            user_id: number;
            uuid: string;
        };
        /** @description NOTE(W2): internal/api/v2/notifications/api.go:405 (`decodeCurrentNotificationBulkDelete`). */
        NotificationBulkDeleteRequest: {
            ids: number[];
        };
        /** @description NOTE(W2): internal/api/v2/notifications/api.go:383 (`decodeCurrentNotificationBulkUpdate`). */
        NotificationBulkSeenRequest: {
            /** @description The string `all`, or an array of notification ids. */
            ids: string | number[];
            is_seen: boolean;
        };
        /** @description NOTE(W2): the notification routes write the pylon `{"ok": false, "error": ...}` envelope on every failure, including "Notification is not found", which answers 400 rather than 404 (internal/api/v2/notifications/api.go:479-491). */
        NotificationErrorResponse: {
            error: string;
            ok: boolean;
        };
        /** @description `total` alone with `only_total=true`; `total` and `rows` otherwise. With `changes_since` (ADR-0025 WP6) `tombstones`, `next_cursor` and `has_more` are added and `total` counts all of the caller's notifications. NOTE(W2): internal/api/v2/notifications/api.go:202 and :561. */
        NotificationListing: {
            /** @description Delta mode only. See ConversationListing.has_more. */
            has_more?: boolean;
            /** @description Delta mode only. See ConversationListing.next_cursor. */
            next_cursor?: string;
            rows?: components["schemas"]["Notification"][];
            /** @description Delta mode only. Notifications deleted since the cursor. */
            tombstones?: components["schemas"]["SyncTombstone"][];
            /** Format: int64 */
            total: number;
        };
        /** @description NOTE(W2): `{"ok": true}` acknowledgements — SetDefaultVersion (internal/api/v2/applications/handler.go:981), BatchReplaceVersion (:994), UpdateAttachmentStorage (internal/api/v2/eliteacore/handler.go:1783), UpdateIcon (:1860), BatchUpdateEntitySettings (internal/api/v2/conversations/handler.go:675), and social/handler.go's UpdateAuthor (:133, 166), Like (:271, plus the no-op shortcuts at :253, 258), Unlike (:304, plus :288, 293), Pin (:324, plus :315), and Unpin (:344, plus :335). */
        OkResponse: {
            ok: boolean;
        };
        /** @description A project member. NOTE(W2): internal/api/v2/conversations/candidates.go:40 (`ParticipantCandidate`). */
        ParticipantCandidate: {
            already_participant: boolean;
            email?: string;
            name: string;
            /**
             * Format: int64
             * @description The USER id — `entity_meta.id` of a user participant and a `user_ids` mention entry.
             */
            user_id: number;
        };
        /** @description One page of participant candidates. NOTE(W2): internal/api/v2/conversations/candidates.go:75 (`participantCandidatesPage`). */
        ParticipantCandidatesPage: {
            has_more: boolean;
            /** @description Pass as `cursor` for the next page; null on the last page. */
            next_cursor: string | null;
            rows: components["schemas"]["ParticipantCandidate"][];
        };
        PipelineSettings: {
            [key: string]: unknown;
        };
        /** @description NOTE(W2): internal/api/v2/projects/handler.go — the Group struct. */
        ProjectGroup: {
            id: number;
            name: string;
        };
        /** @description NOTE(W2): the exact object internal/api/v2/eliteacore/handler.go's normalizeProjectIconMeta produces — the two keys `name` and `url`, each of which may be null, and NO other key survives the write. A null `icon_meta` means the project has no icon. */
        ProjectIconMeta: {
            /** @description The stored file name, or null. */
            name?: string | null;
            /** @description The stored URL, or null. Nothing here invents one. */
            url?: string | null;
        } | null;
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
        /** @description NOTE(W2): publicApplicationDetail response, internal/api/v2/eliteacore/handler.go:1477-1483. */
        PublicApplicationDetail: {
            description: string;
            id: string;
            name: string;
            version_details: components["schemas"]["ApplicationVersionDetail"];
        };
        /** @description NOTE(W2): internal/api/v2/eliteacore/handler.go:1316. */
        PublicApplicationList: {
            rows: components["schemas"]["PublicApplicationSummary"][];
            total: number;
        };
        /** @description NOTE(W2): internal/api/v2/eliteacore/public_applications.go — PublicApplications. */
        PublicApplicationSummary: {
            agent_type: string;
            description: string;
            id: string;
            /** @description Whether the CALLER has liked this application. Always false when the request carries no user identity. */
            is_liked: boolean;
            /** @description How many people have liked this application, counted over the public project's social_likes rows for entity `application`. */
            likes: number;
            meta: components["schemas"]["VersionMeta"] | null;
            name: string;
            /** @description Always the public project id. */
            project_id: string;
            /** @description Tags of the published version. Empty when the version has none; never null. */
            tags: components["schemas"]["PublicApplicationTag"][];
            version_id: string;
            version_name: string;
        };
        /** @description One tag of the published version, as the catalogue row carries it (internal/api/v2/eliteacore/public_applications.go). Only the id and the name are aggregated — the tag's `data` jsonb is not read here. */
        PublicApplicationTag: {
            id: number;
            name: string;
        };
        /** @description NOTE(W2): internal/api/v2/eliteacore/handler.go:1729-1734. The limit/days query params are accepted but not read by the handler. */
        RecommendationsResponse: {
            applications: {
                description: string;
                id: string;
                likes: number;
                name: string;
            }[];
            total: number;
        };
        /** @description Opaque selected-tools blob (object or null) — see marker. */
        SelectedTools: unknown;
        /** @description NOTE(W2): `config` is never populated in responses, `is_default` (the boolean property, unrelated to `default_version_id`) is always false, `type` is always the literal "skill", and `updated_at` is always the zero sentinel "0001-01-01T00:00:00Z" — the skills table itself has no updated_at/is_default columns (migrations/001_initial.sql). instructions/tags/versions/ version_details/default_version_id, in contrast, DO round-trip. */
        Skill: {
            config?: {
                [key: string]: unknown;
            };
            /** Format: date-time */
            created_at: string;
            /** @description skills.meta.default_version_id (issue 874), mirroring applications.meta.default_version_id. The version a NEW attachment proposes. Does NOT change which version an EXISTING agent attachment resolves at chat time — that stays keyed off entity_skill_mapping.skill_version_id, fixed at attach time. */
            default_version_id?: string;
            description?: string;
            id: string;
            /** @description The CURRENT version's instructions — `base`'s, unless the request named another version (getSkillVersion, updateSkillVersion) — also available at version_details.instructions. */
            instructions?: string;
            is_default: boolean;
            name: string;
            project_id: string;
            /** @description The current version's tags (also available at version_details.tags). */
            tags?: string[];
            type: string;
            /** Format: date-time */
            updated_at: string;
            /** @description The CURRENT version (see `instructions`), not always versions[0]. */
            version_details?: components["schemas"]["SkillVersion"];
            /** @description EVERY version of the skill (issue 874), `base` first — not just `base`, as before issue 874. getSkill/getSkillVersion both carry the full set; there is no separate list-versions endpoint. */
            versions?: components["schemas"]["SkillVersion"][];
        };
        /** @description Returned whole by List (internal/api/v2/skills/handler.go). Pagination comes from ?page/?page_size; ?query does an ILIKE search over name/description, and ?sort_by (name|created_at)/?sort_order (asc|desc) drive ORDER BY (repos/skills.go List — sort_by is whitelisted, never interpolated raw). */
        SkillsList: {
            items: components["schemas"]["Skill"][];
            page: number;
            page_size: number;
            total: number;
            total_pages: number;
        };
        /** @description A skill's content: the instructions an agent follows plus its topical tags. Persisted in skill_versions/skill_version_tag_association (repos/skills.go). Since issue 874 one skill_versions row is no longer the whole story — see Skill.versions. */
        SkillVersion: {
            /** Format: date-time */
            created_at?: string;
            id?: string;
            instructions: string;
            /** @description Whether this is the version named by Skill.default_version_id — the version a NEW attachment proposes. Does not affect which version an EXISTING agent attachment resolves at chat time; see Skill.default_version_id. */
            is_default?: boolean;
            /** @description "base" is the always-present working copy every skill has had since creation, and is reserved — it cannot be deleted or reused as a NAMED version's name. Since issue 874 a skill may carry additional named versions alongside it, created by createSkillVersion. */
            name: string;
            /** @description The version this one was cloned from (createSkillVersion) or restored from (restoreSkillVersion). Absent when the row has no recorded ancestor. */
            parent_version_id?: string;
            /** @description "draft" or "published" (#249). A published version is frozen: updateSkillVersion and deleteSkillVersion both refuse it with 409, mirroring application_versions' own guard. */
            status?: string;
            tags: string[];
        };
        /** @description NOTE(W2): AuthorResponse struct (internal/api/v2/social/handler.go:41-49), served by GetAuthor (:51-123). All string fields except personalization are present with a `""` fallback on both the "row found" and "no row / query error" paths (:98-121) — the query error path is a swallowed fallback, always 200, never surfaced as an error. */
        SocialAuthorProfile: {
            avatar: string;
            default_context_management?: components["schemas"]["MemoryContextManagement"];
            default_summarization?: components["schemas"]["MemorySummarization"];
            description: string;
            email: string;
            /** @description NOTE(W2): scanned from su.user_id (social/handler.go:88) but unconditionally overwritten with intToStr(0) at :114, then replaced with the auth-context user.ID at :118-120 whenever it reads "0" or "" — which it always does. Bug-for-bug: this field is ALWAYS auth.User.ID, never the DB row's su.user_id. */
            id: string;
            name: string;
            personal_project_id: string;
            /** @description Arbitrary user-defined personalization payload. */
            personalization?: unknown;
            /** @description The caller's own federated identity references, exactly as public.auth_core__user_provider.provider_ref stores them (`oidc:<sub>`, `saml:<nameid>`, or a pylon-era bare subject). An operator copies one of these into the deployment's `identity.initial_global_admins` to make a global administrator, which previously needed a SELECT against the production database. Absent when the account holds none: a Form password login creates no provider row. Never another user's — the read is keyed on the authenticated principal's own user id. */
            provider_refs?: string[];
        };
        StaticPipelineContinuationBody: components["schemas"]["StaticPipelineRootContinuation"] | components["schemas"]["StaticPipelineToolsContinuation"];
        StaticPipelineContinuationReceipt: {
            command_id: string;
            created: boolean;
            events_url: string;
            execution_id: string;
            /** Format: uuid */
            response_message_id: string;
            task_id: string;
        };
        StaticPipelineLeafDecision: {
            /** @enum {string} */
            action: "continue";
            child_thread_id: string;
            pause_id: string;
            tool_call_id: string;
            value: string;
        };
        /** @description Continue the original before/after occurrence. Main resolves its frozen checkpoint and version; the browser supplies neither. */
        StaticPipelineRootContinuation: {
            /** Format: uuid */
            conversation_uuid: string;
            /** Format: uuid */
            message_id: string;
            project_id: number;
            static_pause_id: string;
            thread_id?: string;
            user_input: string;
        };
        /** @description Continue only selected original saved-pipeline leaves. Main atomically retains every unselected occurrence. */
        StaticPipelineToolsContinuation: {
            /** Format: uuid */
            conversation_uuid: string;
            /** Format: uuid */
            message_id: string;
            project_id: number;
            static_decisions: components["schemas"]["StaticPipelineLeafDecision"][];
            thread_id?: string;
        };
        /** @description A refused `changes_since` request (ADR-0025 WP6). `error` is `invalid_sync_cursor` (400: the cursor does not decode, or was issued for another list, scope or filter), `invalid_limit` (400) or `sync_cursor_expired` (410: the cursor is older than the tombstone retention, 97 days; resync with `changes_since=0`). NOTE(W2): internal/application/changesync/changesync.go (`ErrorBody`). */
        SyncCursorError: {
            error: string;
            message: string;
        };
        /** @description One removal in a `changes_since` delta (ADR-0025 WP6). NOTE(W2): internal/application/changesync/changesync.go (`Tombstone`). */
        SyncTombstone: {
            /** Format: date-time */
            deleted_at: string;
            /**
             * Format: int64
             * @description The removed item's integer id (for a message, the message group id).
             */
            id: number;
            /** @enum {string} */
            reason: "deleted" | "access_lost";
            /** @description The removed item's uuid; null only where none was stored. */
            uuid: string | null;
        };
        /** @description NOTE(W2): ListToolkits row (internal/api/v2/toolkits/handler.go:811-860). id/type/name are `elitea_tools` columns (internal/infra/db/migrations/001_initial.sql:177-179); description is COALESCE'd to `''` (never null, handler.go:822). created_at and author_id match the migration's `created_at TIMESTAMP NOT NULL` / `author_id INTEGER NOT NULL` (migration :182-183). The listing query intentionally selects only those migrated columns. */
        ToolkitInstance: {
            author_id: number;
            /** Format: date-time */
            created_at: string;
            description: string;
            id: string;
            meta: {
                [key: string]: unknown;
            };
            name: string;
            settings: {
                [key: string]: unknown;
            };
            type: string;
        };
        /** @description NOTE(W2): `{rows, total}` envelope (internal/api/v2/toolkits/handler.go:526-534). Repository failures are returned as a safe 500 error rather than an indistinguishable empty successful listing. */
        ToolkitInstanceListResponse: {
            rows: components["schemas"]["ToolkitInstance"][];
            total: number;
        };
        /** @description Opaque tool/toolkit settings blob — see marker. */
        ToolSettings: unknown;
        /** @description The body of updateConversation. Every key is optional and only the keys present are written; a native client renames with `name` alone. NOTE(W2): internal/api/v2/conversations/handler.go (`Update`, `conversationNameFromBody`) reads these keys and ignores any other. */
        UpdateConversationRequest: {
            /** @description One of the caller's own folders; `null` takes it out of its folder. */
            folder_id?: number | null;
            instructions?: string;
            /** @description `false` publishes the conversation in its project. Making a public conversation private is the author's alone. */
            is_private?: boolean;
            /** @description Replaces the whole settings document when present. */
            meta?: {
                [key: string]: unknown;
            };
            /** @description The new title, trimmed; 1 to 256 characters after trimming, no control characters. `null` does not rename. */
            name?: string | null;
        };
        /** @description A conversation as updateConversation answers it. `id` is the numeric id AS A STRING, as in createConversation's answer; `uuid` is the id the chat send path takes. NOTE(W2): internal/infra/db/repos/conversations.go:685 (`Update`) fills these from the UPDATE's RETURNING row. Any other key it carries (a `message_count` of 0, which is not counted here) is not part of the answer's meaning. */
        UpdatedConversation: {
            /** Format: date-time */
            created_at: string;
            created_by: string;
            folder_id?: string | null;
            id: string;
            instructions?: string;
            is_private: boolean;
            meta?: {
                [key: string]: unknown;
            };
            name: string;
            project_id: string;
            /** Format: date-time */
            updated_at: string;
            /** Format: uuid */
            uuid: string;
        };
        UsageDailyEntry: {
            /** Format: int64 */
            api_requests: number;
            /** Format: int64 */
            completion_tokens: number;
            /**
             * Format: date
             * @description The UTC calendar day, the zone billing periods are computed in.
             */
            date: string;
            /** Format: int64 */
            prompt_tokens: number;
            /**
             * @description USD for the day. REMOVED for a caller who may not see amounts, like
             *     every other money field.
             */
            spend?: number | null;
            /** Format: int64 */
            total_tokens: number;
        };
        /**
         * @description The per-model, per-day and per-token views, from the per-request usage
         *     ledger (issue #320). Before that ledger existed the platform kept one
         *     accumulated figure per scope and period, and these views could not be
         *     populated at all.
         *
         *     Every field except `usage_events_available` is ABSENT when the ledger
         *     holds no row for the scope in this period. Read the flag first.
         */
        UsageDimensions: {
            /**
             * Format: int64
             * @description Billed requests in the period.
             */
            api_requests?: number;
            /** Format: int64 */
            completion_tokens?: number;
            /**
             * @description One entry per day that had at least one call. Days with no calls are
             *     omitted rather than zero-filled: the period runs to the end of the
             *     calendar month, and a zero for a day nobody could have used yet is a
             *     claim about the future.
             */
            daily?: components["schemas"]["UsageDailyEntry"][];
            /** @description One entry per (provider, model) billed in the period, ordered by spend. */
            models?: components["schemas"]["UsageModelEntry"][];
            /**
             * Format: int64
             * @description Provider-reported prompt tokens for the period. Never estimated.
             */
            prompt_tokens?: number;
            /** Format: int64 */
            total_tokens?: number;
            /**
             * @description Whether the ledger holds any row for this scope in this period.
             *     FALSE means no data, which is NOT the same claim as no calls — the
             *     accumulated `spend` may still be non-zero for a period that began
             *     before the ledger did.
             */
            usage_events_available: boolean;
        };
        UsageModelEntry: {
            /** Format: int64 */
            api_requests: number;
            /** Format: int64 */
            completion_tokens: number;
            model: string;
            /** Format: int64 */
            prompt_tokens: number;
            provider: string;
            /** @description USD for the model. REMOVED for a caller who may not see amounts. */
            spend?: number | null;
            /** Format: int64 */
            total_tokens: number;
        };
        UsageReport: {
            /**
             * @description Present for scope=project only.
             * @enum {string}
             */
            budget_period?: "monthly";
            /** @description Present for scope=user only. See MemberBudget.enforced. */
            enforced?: boolean;
            project_id: number;
            /** @enum {string} */
            scope: "project" | "user";
            /** @description Null for scope=project. */
            user_id: number | null;
        } & components["schemas"]["UsageDimensions"] & components["schemas"]["BudgetState"];
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
        /** @description 426 Upgrade Required: the native client's `X-Client-Version` is below the minimum this deployment serves it; `X-Min-Client-Version` repeats the minimum. Deliberately WITHOUT the `Upgrade` header RFC 9110 requires on a 426: the upgrade is of the app, not of a protocol. */
        ClientUpgradeRequired: {
            headers: {
                "X-Min-Client-Version"?: string;
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["ClientUpgradeRequiredError"];
            };
        };
        /** @description 400: a native client's `X-Client-Version` does not parse as MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Only callers authenticated by a native access token are judged. */
        InvalidClientVersion: {
            headers: {
                [name: string]: unknown;
            };
            content: {
                "application/json": components["schemas"]["InvalidClientVersionError"];
            };
        };
        /** @description 410 Gone: the `changes_since` cursor is older than the tombstone retention; the client discards its cache for this list and resyncs with `changes_since=0`. */
        SyncCursorExpired: {
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
        /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
        XClientVersion: string;
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
            /** @description The discovery document. */
            200: {
                headers: {
                    "Cache-Control"?: string;
                    ETag?: string;
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["DiscoveryDocument"];
                };
            };
            /** @description If-None-Match matched the current entity tag. */
            304: {
                headers: {
                    ETag?: string;
                    [name: string]: unknown;
                };
                content?: never;
            };
            /** @description A source of the document is unavailable; retry. */
            503: {
                headers: {
                    "Retry-After"?: string;
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
        };
    };
    listNativeDevices: {
        parameters: {
            query?: {
                include_revoked?: boolean;
            };
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The caller's devices. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NativeDeviceList"];
                };
            };
            400: components["responses"]["InvalidClientVersion"];
            401: components["responses"]["401"];
            /** @description No native client is registered on this deployment. */
            404: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NativeOAuthError"];
                };
            };
            426: components["responses"]["ClientUpgradeRequired"];
        };
    };
    revokeNativeDevice: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                device_id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Revoked. */
            204: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            400: components["responses"]["InvalidClientVersion"];
            401: components["responses"]["401"];
            /** @description Not one of the caller's devices. */
            404: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NativeOAuthError"];
                };
            };
            426: components["responses"]["ClientUpgradeRequired"];
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
            /** @description invalid_client (the client is not registered). */
            401: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NativeOAuthError"];
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
            /** @description Too many requests from this address; see Retry-After. */
            429: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NativeOAuthError"];
                };
            };
            /** @description temporarily_unavailable. */
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
    exchangeNativeToken: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
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
            /** @description A new token pair. Cache-Control is no-store. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NativeTokenResponse"];
                };
            };
            /** @description invalid_request (including an X-Client-Version that does not parse), unsupported_grant_type or invalid_grant. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NativeOAuthError"];
                };
            };
            /** @description invalid_client (the client is unknown or disabled), or device_revoked: the refresh token's device session was revoked, expired, deactivated or reused, and the client must wipe its local data. */
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
            426: components["responses"]["ClientUpgradeRequired"];
            /** @description Too many failed requests; see Retry-After. */
            429: {
                headers: {
                    "Retry-After"?: string;
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NativeOAuthError"];
                };
            };
            /** @description temporarily_unavailable; retry after Retry-After. */
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
                /** @description Content version token — the current ETag value without quotes, as `brand_pack_url` in the discovery document carries it. */
                v?: string;
            };
            header?: never;
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The resolved brand pack. */
            200: {
                headers: {
                    "Cache-Control"?: string;
                    /** @description Strong entity tag of this response body. */
                    ETag?: string;
                    /** @description Comma-separated contributing layers (`default`, `file`, `db`). */
                    "X-Elitea-Brand-Layers"?: string;
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["BrandPack"];
                };
            };
            /** @description The `?v=` token does not match the current ETag value; Location points at the current-version URL. */
            302: {
                headers: {
                    Location?: string;
                    [name: string]: unknown;
                };
                content?: never;
            };
            /** @description If-None-Match matched the current entity tag. */
            304: {
                headers: {
                    ETag?: string;
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
    listApplicationSkills: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                app_version_id: number;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Skills list */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ApplicationSkillsList"];
                };
            };
            400: components["responses"]["400"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            426: components["responses"]["ClientUpgradeRequired"];
        };
    };
    getApplication: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
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
            400: components["responses"]["InvalidClientVersion"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
            426: components["responses"]["ClientUpgradeRequired"];
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
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
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
            400: components["responses"]["InvalidClientVersion"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            426: components["responses"]["ClientUpgradeRequired"];
        };
    };
    uploadConversationAttachment: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description The conversation UUID. A numeric id is refused with 400: the file is stored as `{conversation_uuid}/{file name}`. */
                conversation_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "multipart/form-data": {
                    /** @description Chunked upload only; 0-based. */
                    chunk_index?: number;
                    /** Format: binary */
                    file: string;
                    /** @description Chunked upload only; the same for every chunk of one file. */
                    file_id?: string;
                    /** @description Defaults to the part's file name; directory parts are stripped. */
                    file_name?: string;
                    /** @description Chunked upload only. */
                    total_chunks?: number;
                };
            };
        };
        responses: {
            /** @description The file is stored. */
            201: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ConversationAttachmentUploaded"][];
                };
            };
            /** @description A chunk was stored; send the next one. */
            202: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ConversationAttachmentChunkReceived"];
                };
            };
            400: components["responses"]["400"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    downloadConversationAttachment: {
        parameters: {
            query?: never;
            header?: {
                "If-None-Match"?: string;
                Range?: string;
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description The conversation UUID (a numeric id is a 400), as in the attachment's `filepath`. */
                conversation_id: string;
                /** @description The file name: the last segment of the attachment's `filepath` (`/{bucket}/{conversation_uuid}/{name}`), percent-encoded. A name with a slash, a backslash, `..` or a control character is a 400. */
                name: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The whole file. */
            200: {
                headers: {
                    "Content-Disposition"?: string;
                    ETag?: string;
                    [name: string]: unknown;
                };
                content: {
                    "application/octet-stream": string;
                };
            };
            /** @description The requested range. */
            206: {
                headers: {
                    "Content-Range"?: string;
                    [name: string]: unknown;
                };
                content: {
                    "application/octet-stream": string;
                };
            };
            /** @description Not modified (the `If-None-Match` ETag matches). */
            304: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            /** @description A numeric conversation id, an invalid file name, or an `X-Client-Version` that does not parse. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
            /** @description The range starts past the end of the file; `Content-Range` states its length. */
            416: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
            /** @description A composition with no object store. */
            501: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
        };
    };
    continueChatExecution: {
        parameters: {
            query: {
                execution_contract: "agent.continue.hitl.v1" | "agent.continue.authorization.v1" | "agent.continue.output-limit.v1" | "agent.continue.static.v1";
            };
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description The conversation UUID; must equal the body's `conversation_uuid`. */
                conversation_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ChatContinueRequest"] & (components["schemas"]["StaticPipelineContinuationBody"] | components["schemas"]["ChatContinueRequest"]);
            };
        };
        responses: {
            /** @description The continuation was admitted or replayed. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionStarted"] & (components["schemas"]["StaticPipelineContinuationReceipt"] | components["schemas"]["ChatExecutionStarted"]);
                };
            };
            /** @description A malformed request. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionError"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            /** @description The pause was already answered. */
            409: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionError"];
                };
            };
            /** @description The body is larger than 512 KiB. */
            413: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionError"];
                };
            };
            /** @description Content-Type is not application/json. */
            415: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionError"];
                };
            };
            /** @description `unsupported_agent_execution`: the fields do not fit the contract. */
            422: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionError"];
                };
            };
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
            /** @description Admission capacity is full; retry after `Retry-After`. */
            503: {
                headers: {
                    "Retry-After"?: string;
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionError"];
                };
            };
        };
    };
    exportConversation: {
        parameters: {
            query?: {
                /** @description `md` (the default) is the readable document — a title, a short header block and one section per message. `json` is the faithful one, carrying every field the transcript read returns. Any other value is a 400 rather than a silent fallback, because a client that asked for a format it did not get would save the wrong extension. */
                format?: "md" | "markdown" | "json";
            };
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description The conversation's numeric id or its UUID. Both resolve, the same way the conversation detail read resolves them. */
                conversation_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The transcript document. `text/markdown` for `format=md`, `application/json` for `format=json`. */
            200: {
                headers: {
                    /** @description `attachment` plus the derived filename. Exposed to browser fetch through Access-Control-Expose-Headers, because the web client names the saved file from it. */
                    "Content-Disposition"?: string;
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ConversationExport"];
                    "text/markdown": string;
                };
            };
            /** @description An unsupported format, or a conversation too long to export in one document. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    getConversation: {
        parameters: {
            query?: {
                editor_test_runs?: boolean;
                /** @description Embed this many message groups (0 or absent embeds none). */
                messages_limit?: number;
                messages_offset?: number;
                runs_limit?: number;
                runs_offset?: number;
                /** @description Order of the embedded groups, `asc` or `desc`. */
                sort_order?: string;
            };
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description The conversation's numeric id or its UUID; both resolve. */
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
            400: components["responses"]["InvalidClientVersion"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    updateConversation: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description The conversation's numeric id or its UUID; both resolve. */
                conversation_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["UpdateConversationRequest"];
            };
        };
        responses: {
            /** @description The updated conversation. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["UpdatedConversation"];
                };
            };
            /** @description A body that is not a JSON object, an invalid `name` or `instructions`, a privacy change only the author may make, or a native client version that does not parse (invalid_client_version). */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"] | components["schemas"]["InvalidClientVersionError"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    deleteConversation: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description The conversation's numeric id or its UUID; both resolve. */
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
            400: components["responses"]["InvalidClientVersion"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    listConversations: {
        parameters: {
            query?: {
                /** @description Incremental sync (ADR-0025 WP6). Absent: the legacy page, unchanged. Empty or `0`: a full sync from the beginning in delta shape. Otherwise the opaque `next_cursor` of a previous delta of THIS list — the same project and the same `source`, `entity_name`, `entity_meta_id`, `mine` and `hidden` values; a cursor from another list is a 400. Rows come oldest change first. Losing access to the whole project is not a tombstone: the client sees 403 and drops that project's cache. */
                changes_since?: string;
                /** @description The participant entity's id, paired with entity_name. */
                entity_meta_id?: string;
                /** @description `application` (agents and pipelines) or `toolkit` (toolkits and MCP connections). */
                entity_name?: string;
                /** @description `only` inverts the default hidden-conversation exclusion; any other value keeps the default. */
                hidden?: string;
                /** @description Page size, at most 100. In delta mode the default is 100, and rows and tombstones are each bounded by it. */
                limit?: number;
                mine?: boolean;
                /** @description Legacy paging only; ignored with `changes_since`. */
                offset?: number;
                /** @description Filter by `chat_conversations.source` (e.g. `deepwiki`). */
                source?: string;
            };
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The matching conversations, newest first; with `changes_since`, the conversations changed since the cursor, oldest change first, plus tombstones and the next cursor. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ConversationListing"];
                };
            };
            /** @description `changes_since` is not a cursor this list issued, or `limit` is not a positive integer (SyncCursorError); or, for a native client, `X-Client-Version` does not parse (InvalidClientVersionError). */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["SyncCursorError"] | components["schemas"]["InvalidClientVersionError"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            410: components["responses"]["SyncCursorExpired"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    createConversation: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ConversationCreateRequest"];
            };
        };
        responses: {
            /** @description The created conversation. */
            201: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["CreatedConversation"];
                };
            };
            400: components["responses"]["400"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    listMemories: {
        parameters: {
            query?: {
                /** @description Case-insensitive substring filter over content and tags. */
                q?: string;
            };
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The caller's own memories in this project. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["MemoryEntryList"];
                };
            };
            400: components["responses"]["InvalidClientVersion"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    clearMemories: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description How many memories were removed. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        removed: number;
                    };
                };
            };
            400: components["responses"]["InvalidClientVersion"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    deleteMemory: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                memory_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Deleted (or already absent). */
            204: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            400: components["responses"]["InvalidClientVersion"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    getMessageFeedback: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description The message-GROUP uuid — the same `{messageID}` GetMessage and DeleteMessage share (internal/api/v2/conversations/handler.go). */
                message_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Feedback summary for the message. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["MessageFeedbackSummary"];
                };
            };
            400: components["responses"]["InvalidClientVersion"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    setMessageFeedback: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description The message-GROUP uuid — the same `{messageID}` GetMessage and DeleteMessage share (internal/api/v2/conversations/handler.go). */
                message_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["MessageFeedbackRequest"];
            };
        };
        responses: {
            /** @description Feedback recorded; the updated summary. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["MessageFeedbackSummary"];
                };
            };
            /** @description rating was not 1 or -1, or the comment exceeded the length limit. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            /** @description No such message in this project. */
            404: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    deleteMessageFeedback: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description The message-GROUP uuid — the same `{messageID}` GetMessage and DeleteMessage share (internal/api/v2/conversations/handler.go). */
                message_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Feedback retracted; the updated summary. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["MessageFeedbackSummary"];
                };
            };
            400: components["responses"]["InvalidClientVersion"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    getMessageTrace: {
        parameters: {
            query: {
                /** @description Original client generation. All three identity fields are required together and are checked in the trace read snapshot. */
                execution_generation?: string;
                /** @description Original admitted execution. Supply with execution_generation, response_message_id and message_group_id. Mismatched or superseded identity returns no trace. */
                execution_id?: string;
                /** @description The owning message group, from the trace-list row. */
                message_group_id: number;
                /** @description Original response UUID. The execution binding must belong to the authenticated actor and exact project. Ordinary callers may omit all identity fields. */
                response_message_id?: string;
            };
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
                step_id: number;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The full trace step. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["MessageTraceStepDetail"];
                };
            };
            /** @description message_group_id was missing or not a positive integer. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            /** @description No such step within that message group. */
            404: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    listMessageTraces: {
        parameters: {
            query?: {
                /** @description Original client generation. All three identity fields are required together and are checked in the trace read snapshot. */
                execution_generation?: string;
                /** @description Original admitted execution. Supply with execution_generation, response_message_id and message_group_id. Mismatched or superseded identity returns no trace. */
                execution_id?: string;
                include_total?: boolean;
                /** @description Filter by step kind. Allowed values are `tool_call` and `thinking_step`; anything else is a 400 rather than an empty page. */
                kind?: string;
                limit?: number;
                /** @description Scope to a single message group. A value that is not a positive integer is a 400 rather than an ignored filter — dropping it would return the whole conversation to a caller that asked for one message. Omit the parameter to ask for the whole conversation. */
                message_group_id?: number;
                /** @description Comma-separated loaded message-group ids, at most 200. Duplicates are collapsed; a non-integer member is a 400. */
                message_group_ids?: string;
                offset?: number;
                /** @description Original response UUID. The execution binding must belong to the authenticated actor and exact project. Ordinary callers may omit all identity fields. */
                response_message_id?: string;
            };
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                conversation_id: number;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Trace steps for the conversation. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["MessageTraceListing"];
                };
            };
            /** @description An unsupported kind, a malformed message_group_id, or a malformed message-group id list. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    listConversationMessages: {
        parameters: {
            query?: {
                /** @description Incremental sync (ADR-0025 WP6), as on listConversations. Empty or `0` is a full sync; otherwise the `next_cursor` of a previous delta of THIS conversation's transcript. */
                changes_since?: string;
                /** @description Page size, at most 100 (default 10; 100 in delta mode, for rows and tombstones each). */
                limit?: number;
                offset?: number;
                /** @description Legacy alias, converted through `limit`; `offset` wins when both are sent. */
                page?: number;
                /** @description Legacy alias of `limit`; `limit` wins. */
                page_size?: number;
                /** @description Literal (not wildcard) text search over message text. */
                query?: string;
                sort_by?: string;
                /** @description Only `asc` flips the default `desc`. */
                sort_order?: string;
            };
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description The conversation. The list takes its numeric id or its UUID; the send takes the UUID only, and it must equal the body's `conversation_uuid`. */
                conversation_id: string;
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
            /** @description `changes_since` is not a cursor this list issued, or `limit` is not a positive integer (SyncCursorError); or, for a native client, `X-Client-Version` does not parse (InvalidClientVersionError). */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["SyncCursorError"] | components["schemas"]["InvalidClientVersionError"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
            410: components["responses"]["SyncCursorExpired"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    sendChatMessage: {
        parameters: {
            query: {
                execution_contract: "agent.execute.application.v1" | "agent.execute.adhoc.v1";
            };
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description The conversation. The list takes its numeric id or its UUID; the send takes the UUID only, and it must equal the body's `conversation_uuid`. */
                conversation_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ChatSendRequest"];
            };
        };
        responses: {
            /** @description The turn was admitted (`created` true) or replayed (`created` false). */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionStarted"];
                };
            };
            /** @description A malformed request, path/body mismatch, an invalid UUID, a bad mention or attachment list. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionError"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            /** @description The `question_id` was already used with a different body. */
            409: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionError"];
                };
            };
            /** @description The body is larger than 512 KiB. */
            413: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionError"];
                };
            };
            /** @description Content-Type is not application/json. */
            415: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionError"];
                };
            };
            /** @description `unsupported_agent_execution`: this turn cannot run on the current execution path. */
            422: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionError"];
                };
            };
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
            /** @description Admission capacity is full; retry after `Retry-After` with the SAME `question_id`. */
            503: {
                headers: {
                    "Retry-After"?: string;
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionError"];
                };
            };
        };
    };
    listParticipantCandidates: {
        parameters: {
            query?: {
                /** @description The previous page's `next_cursor`. */
                cursor?: string;
                limit?: number;
                /** @description Case-insensitive substring of the name or the e-mail. */
                q?: string;
                /** @description The candidate kind. Only `user` is served; it is the default. */
                type?: string;
            };
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description The conversation's numeric id or its UUID; both resolve. */
                conversation_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description One page of candidates. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ParticipantCandidatesPage"];
                };
            };
            /** @description A `type` other than `user`, a `q` over 100 characters, a `limit` out of range, a cursor this list did not issue, or an `X-Client-Version` that does not parse. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
            /** @description A composition without a database; there are no members to list. */
            501: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
        };
    };
    deleteConversationParticipant: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description The conversation's numeric id or its UUID; both resolve. */
                conversation_id: string;
                /** @description The participant row id (`ConversationParticipant.id`), not the entity id. */
                participant_id: number;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Removed. */
            204: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            /** @description The participant is the conversation's author, or an `X-Client-Version` that does not parse. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    addConversationParticipants: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description The conversation's numeric id or its UUID; both resolve. */
                conversation_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ConversationParticipantCreate"] | components["schemas"]["ConversationParticipantCreate"][];
            };
        };
        responses: {
            /** @description The participant rows of this request, in request order. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ConversationParticipant"][];
                };
            };
            /** @description A malformed body, an unknown kind, a missing required key, more than 100 participants, or an `X-Client-Version` that does not parse. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    getPublicApplication: {
        parameters: {
            query?: never;
            header?: never;
            path: {
                application_id: number;
                version_name: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Published application detail */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["PublicApplicationDetail"];
                };
            };
            404: components["responses"]["404"];
        };
    };
    listPublicApplications: {
        parameters: {
            query?: {
                /**
                 * @description One of `all`, `classic` or `pipeline`. `pipeline` keeps only pipeline agents, `classic` keeps everything else, `all` (the default) keeps both. Any other value is refused with 400.
                 *     The allowed set is stated here rather than as an OpenAPI `enum` on purpose: an `enum` of `asc`/`desc` on a second operation makes oapi-codegen rename the FIRST operation's generated constants, which turns a two-parameter spec addition into a repository-wide generated diff. The handler's own allowlist (public_applications.go) is the enforcement point.
                 */
                agents_type?: string;
                /** @description Marketplace category filter, matched against the published version's `meta.category`. The value `Other` also takes in every agent with no category at all. */
                category?: string;
                /** @description Maximum number of items to return */
                limit?: components["parameters"]["Limit"];
                /** @description Keep only the applications the CALLER has liked. A request with no user identity answers no rows rather than the generic list. */
                my_liked?: boolean;
                /** @description Number of items to skip before starting to collect results */
                offset?: components["parameters"]["Offset"];
                /** @description Case-insensitive substring search over the application name and description. */
                query?: string;
                /** @description Sort key — one of `id`, `name`, `created_at` or `likes`. Ties break on the application id descending, so paging stays stable. Any other value is refused with 400. */
                sort_by?: string;
                /** @description `asc` or `desc`. Any other value is refused with 400. */
                sort_order?: string;
                /** @description Comma-separated version statuses (published, draft, on_moderation, rejected, embedded). The catalogue only holds published versions, so this narrows that set; a list without `published` answers no rows. */
                statuses?: string;
            };
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Published applications */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["PublicApplicationList"];
                };
            };
            /** @description A refused query parameter (`error: invalid_query_parameter`, with `param` and `msg`), or a native client version that does not parse (`error: invalid_client_version`). */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["InvalidQueryParameterError"];
                };
            };
            426: components["responses"]["ClientUpgradeRequired"];
        };
    };
    getRecommendations: {
        parameters: {
            query?: {
                /** @description Old-SPA parameter; accepted but not read. */
                days?: number;
                /** @description Maximum number of items to return */
                limit?: components["parameters"]["Limit"];
            };
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Recommended applications */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["RecommendationsResponse"];
                };
            };
            400: components["responses"]["InvalidClientVersion"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            426: components["responses"]["ClientUpgradeRequired"];
        };
    };
    regenerateChatMessage: {
        parameters: {
            query: {
                execution_contract: "agent.regenerate.v1";
            };
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description The assistant answer to regenerate (its `uid`, the send's `response_message_id`). */
                message_id: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["ChatRegenerateRequest"];
            };
        };
        responses: {
            /** @description The regeneration was admitted or replayed. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionStarted"];
                };
            };
            /** @description A malformed request. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionError"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            /** @description `agent_regeneration_pending` (retry), or an idempotency conflict. */
            409: {
                headers: {
                    "Retry-After"?: string;
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionError"];
                };
            };
            /** @description The body is larger than 512 KiB. */
            413: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionError"];
                };
            };
            /** @description Content-Type is not application/json. */
            415: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionError"];
                };
            };
            /** @description `unsupported_agent_execution`. */
            422: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionError"];
                };
            };
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
            /** @description Admission capacity is full; retry after `Retry-After`. */
            503: {
                headers: {
                    "Retry-After"?: string;
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ChatExecutionError"];
                };
            };
        };
    };
    cancelChatExecution: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
                /** @description The running answer to stop (its `uid`, the send's or the regeneration's `response_message_id`). */
                response_message_id: string;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Stopped, or already stopped by this caller. */
            204: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            /** @description An invalid response message id, or a native client version that does not parse (InvalidClientVersionError). */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            /** @description The answer is not running, or this caller may not stop it. */
            409: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
            426: components["responses"]["ClientUpgradeRequired"];
            /** @description The stop could not be recorded. Retry. */
            502: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
            /** @description The stop timed out. Retry. */
            504: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
        };
    };
    listToolkitAvailableTools: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
                /** @description Saved toolkit ID in the authorized project. */
                toolkit_id: number;
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Available tool descriptions and argument schemas. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        args_schemas: {
                            [key: string]: {
                                [key: string]: unknown;
                            };
                        };
                        tools: {
                            description: string;
                            name: string;
                        }[];
                    };
                };
            };
            400: components["responses"]["400"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
            /** @description The selected worker cannot construct this toolkit type. */
            422: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
            /** @description Discovery is unavailable or its durable run has not settled. */
            503: {
                headers: {
                    [name: string]: unknown;
                };
                content?: never;
            };
        };
    };
    listToolkitInstances: {
        parameters: {
            query?: {
                /** @description Maximum number of items to return */
                limit?: components["parameters"]["Limit"];
                /** @description Number of items to skip before starting to collect results */
                offset?: components["parameters"]["Offset"];
            };
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Toolkit instances */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ToolkitInstanceListResponse"];
                };
            };
            400: components["responses"]["InvalidClientVersion"];
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    getProjectUsage: {
        parameters: {
            query?: {
                scope?: "project" | "user";
            };
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Usage and budget state for the requested scope */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["UsageReport"];
                };
            };
            /** @description scope was neither 'project' nor 'user'. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            426: components["responses"]["ClientUpgradeRequired"];
            500: components["responses"]["500"];
        };
    };
    markNotificationSeen: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                notification_id: number;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The notification, now seen. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["Notification"];
                };
            };
            /** @description An invalid id, or a notification that is not the caller's. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationErrorResponse"] | components["schemas"]["InvalidClientVersionError"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            426: components["responses"]["ClientUpgradeRequired"];
            /** @description The write failed. */
            500: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationErrorResponse"];
                };
            };
        };
    };
    deleteNotification: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                notification_id: number;
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
            /** @description An invalid id, a notification that is not the caller's, or a native client version that does not parse (invalid_client_version). */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationErrorResponse"] | components["schemas"]["InvalidClientVersionError"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            426: components["responses"]["ClientUpgradeRequired"];
            /** @description The write failed. */
            500: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationErrorResponse"];
                };
            };
        };
    };
    listNotifications: {
        parameters: {
            query?: {
                /** @description Incremental sync (ADR-0025 WP6). Empty or `0` is a full sync; otherwise the `next_cursor` of a previous notification delta of the same user. */
                changes_since?: string;
                event_type?: string;
                limit?: number;
                offset?: number;
                only_new?: boolean;
                only_total?: boolean;
                search?: string;
                /** @description One of the notification's own fields; default `created_at`. */
                sort_by?: string;
                /** @description `asc` or `desc` (default). */
                sort_order?: string;
            };
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description The page or delta. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationListing"];
                };
            };
            /** @description An invalid query (`{ok, error}`), or a refused delta request (`invalid_sync_cursor`, `invalid_sync_request`, `invalid_limit`). */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationErrorResponse"] | components["schemas"]["SyncCursorError"] | components["schemas"]["InvalidClientVersionError"];
                };
            };
            /** @description Not authenticated. */
            401: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationErrorResponse"];
                };
            };
            403: components["responses"]["403"];
            410: components["responses"]["SyncCursorExpired"];
            426: components["responses"]["ClientUpgradeRequired"];
            /** @description The read failed. */
            500: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationErrorResponse"];
                };
            };
        };
    };
    markNotificationsSeen: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["NotificationBulkSeenRequest"];
            };
        };
        responses: {
            /** @description How many rows changed. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        /** Format: int64 */
                        updated: number;
                    };
                };
            };
            /** @description An invalid body. */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationErrorResponse"] | components["schemas"]["InvalidClientVersionError"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            426: components["responses"]["ClientUpgradeRequired"];
            /** @description The write failed. */
            500: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationErrorResponse"];
                };
            };
        };
    };
    deleteNotifications: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody: {
            content: {
                "application/json": components["schemas"]["NotificationBulkDeleteRequest"];
            };
        };
        responses: {
            /** @description How many rows were deleted. */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": {
                        /** Format: int64 */
                        deleted: number;
                    };
                };
            };
            /** @description An invalid body, or a native client version that does not parse (invalid_client_version). */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationErrorResponse"] | components["schemas"]["InvalidClientVersionError"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            426: components["responses"]["ClientUpgradeRequired"];
            /** @description The write failed. */
            500: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["NotificationErrorResponse"];
                };
            };
        };
    };
    listProjects: {
        parameters: {
            query?: {
                /** @description Presence-only flag. The handler ignores the value, so `check_public_role=false` also switches the check on. */
                check_public_role?: boolean;
            };
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
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
            400: components["responses"]["InvalidClientVersion"];
            401: components["responses"]["401"];
            426: components["responses"]["ClientUpgradeRequired"];
        };
    };
    getCurrentAuthor: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path?: never;
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Author profile */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["SocialAuthorProfile"];
                };
            };
            400: components["responses"]["400"];
            401: components["responses"]["401"];
            426: components["responses"]["ClientUpgradeRequired"];
        };
    };
    pinEntity: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                entity_id: number;
                entity_type: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Pinned or updated the shared project pin */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["OkResponse"];
                };
            };
            /** @description An unknown entity type, an invalid id, or a native client version that does not parse (invalid_client_version). */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"] | components["schemas"]["InvalidClientVersionError"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
            426: components["responses"]["ClientUpgradeRequired"];
            /** @description Pin storage failed */
            500: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
            /** @description Pin storage or project authorization is unavailable */
            503: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
        };
    };
    unpinEntity: {
        parameters: {
            query?: never;
            header?: {
                /** @description The native client's version, MAJOR.MINOR.PATCH with an optional -prerelease (ADR-0025 decision 5). Judged only for a caller authenticated by a native access token (and on the native token endpoint): below the minimum answers 426, a value that does not parse answers 400. Ignored for every other caller. */
                "X-Client-Version"?: components["parameters"]["XClientVersion"];
            };
            path: {
                entity_id: number;
                entity_type: string;
                /** @description Project identifier (extracted from URL path) */
                project_id: components["parameters"]["ProjectId"];
            };
            cookie?: never;
        };
        requestBody?: never;
        responses: {
            /** @description Removed the shared pin or confirmed it is absent */
            200: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["OkResponse"];
                };
            };
            /** @description An unknown entity type, an invalid id, or a native client version that does not parse (invalid_client_version). */
            400: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"] | components["schemas"]["InvalidClientVersionError"];
                };
            };
            401: components["responses"]["401"];
            403: components["responses"]["403"];
            404: components["responses"]["404"];
            426: components["responses"]["ClientUpgradeRequired"];
            /** @description Unpin storage failed */
            500: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
            /** @description Pin storage or project authorization is unavailable */
            503: {
                headers: {
                    [name: string]: unknown;
                };
                content: {
                    "application/json": components["schemas"]["ErrorResponse"];
                };
            };
        };
    };
}
