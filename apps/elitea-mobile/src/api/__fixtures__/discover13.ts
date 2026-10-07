/**
 * Client contract 1.3 Discover shapes, CAPTURED 2026-10-07 from the zefir-wave
 * stack (elitea-platform client contract 1.3, the same document as the v4.7.0
 * pin) as Alice (user 8). The public project is 1; Alice's personal project is
 * 90501. The bodies are verbatim; only the variable names are ours. For the
 * capture the Travel Planner's published version was given an icon (as the
 * admin, through upload_icon) and two tags (rows in the public tenant's tags
 * table), so that both shapes are real.
 */

export const DISCOVER_CAPTURE = { publicProjectId: 1, projectId: 90501 } as const;

/** listPublicApplications `limit=20&offset=0`. */
export const publicList = {
  "rows": [
    {
      "agent_type": "openai",
      "description": "Zefir Travel Planner: a public agent anyone can find in Discover.",
      "id": "2",
      "is_liked": false,
      "likes": 0,
      "meta": {
        "category": "Project Management",
        "icon_meta": {
          "height": 64,
          "url": "/icons/1/076958cfcd17545b.png",
          "width": 64
        },
        "published_by": 10,
        "source_project_id": 90502,
        "source_version_id": "2",
        "step_limit": 25
      },
      "name": "Zefir Travel Planner",
      "project_id": "1",
      "tags": [
        {
          "id": 2,
          "name": "planning"
        },
        {
          "id": 1,
          "name": "travel"
        }
      ],
      "version_id": "2",
      "version_name": "v1"
    },
    {
      "agent_type": "openai",
      "description": "Zefir Concierge: a public agent anyone can find in Discover.",
      "id": "1",
      "is_liked": false,
      "likes": 0,
      "meta": {
        "category": "Other",
        "published_by": 8,
        "source_project_id": 90504,
        "source_version_id": "6",
        "step_limit": 25
      },
      "name": "Zefir Concierge",
      "project_id": "1",
      "tags": [],
      "version_id": "1",
      "version_name": "v1"
    }
  ],
  "total": 2
};

/** listPublicApplications `limit=1&offset=1&sort_by=name&sort_order=asc`: `total` counts the whole filtered set. */
export const publicPage2 = {
  "rows": [
    {
      "agent_type": "openai",
      "description": "Zefir Travel Planner: a public agent anyone can find in Discover.",
      "id": "2",
      "is_liked": false,
      "likes": 0,
      "meta": {
        "category": "Project Management",
        "icon_meta": {
          "height": 64,
          "url": "/icons/1/076958cfcd17545b.png",
          "width": 64
        },
        "published_by": 10,
        "source_project_id": 90502,
        "source_version_id": "2",
        "step_limit": 25
      },
      "name": "Zefir Travel Planner",
      "project_id": "1",
      "tags": [
        {
          "id": 2,
          "name": "planning"
        },
        {
          "id": 1,
          "name": "travel"
        }
      ],
      "version_id": "2",
      "version_name": "v1"
    }
  ],
  "total": 2
};

/** listPublicApplications `query=nomatch`. */
export const publicEmpty = {
  "rows": [],
  "total": 0
};

/** listPublicApplications `sort_by=nope`: 400 with the parameter named. */
export const publicBadParam = {
  "error": "invalid_query_parameter",
  "msg": "must be one of id, name, created_at, likes",
  "param": "sort_by"
};

/** getPublicApplication `2/v1`. */
export const publicDetail = {
  "description": "Zefir Travel Planner: a public agent anyone can find in Discover.",
  "id": "2",
  "name": "Zefir Travel Planner",
  "version_details": {
    "agent_type": "openai",
    "application_id": "2",
    "author_id": "10",
    "conversation_starters": [
      "What can you do?",
      "Give me three ideas"
    ],
    "id": "2",
    "instructions": "You are a concise, cheerful assistant for anyone who finds you in Discover. Greet the user, ask one clarifying question when the request is ambiguous, then answer in at most five short bullet points. Never invent facts; say so when you do not know. Keep a friendly, professional tone.",
    "llm_settings": {
      "max_tokens": 1024,
      "model_name": "vllm/E2E-MOCK-MODEL",
      "model_project_id": 1,
      "temperature": 0.2
    },
    "meta": {
      "category": "Project Management",
      "icon_meta": {
        "height": 64,
        "url": "/icons/1/076958cfcd17545b.png",
        "width": 64
      },
      "published_by": 10,
      "source_project_id": 90502,
      "source_version_id": "2",
      "step_limit": 25
    },
    "name": "v1",
    "pipeline_settings": {},
    "status": "published",
    "tags": [],
    "tools": [],
    "welcome_message": "Welcome to Zefir Travel Planner!"
  }
};

/** getPublicApplication for an id that is not published: 404. */
export const publicDetailMissing = { "error": "application not found" };

/** createConversation in Alice's project, no participants yet (step 1 of starting a chat). */
export const createdConversation = {
  "id": "9",
  "uuid": "71bc5f54-db98-44c6-bdd0-a71fa3015f21",
  "project_id": "90501",
  "name": "Zefir Travel Planner",
  "created_at": "2026-10-07T03:57:25.376984Z",
  "updated_at": "2026-10-07T03:57:25.376984Z",
  "created_by": "8",
  "message_count": 0,
  "is_private": true,
  "meta": {
    "context_strategy": {
      "budget_mode": "balanced",
      "enable_context_editing": false,
      "enable_summarization": true,
      "enabled": true,
      "max_context_tokens": 0,
      "name": "default",
      "preserve_recent_messages": 5,
      "preserve_system_messages": true,
      "summary_instructions": "Generate a concise summary of the following conversation messages",
      "summary_llm_settings": null
    }
  },
  "source": "elitea",
  "participants": [
    {
      "id": 1,
      "entity_name": "user",
      "entity_meta": {
        "id": 8
      },
      "entity_settings": {},
      "meta": {}
    },
    {
      "id": 2,
      "entity_name": "dummy",
      "entity_meta": {},
      "entity_settings": {},
      "meta": {
        "name": "EliteA"
      }
    }
  ]
};

/** addConversationParticipants with the public agent (step 2): `project_id` 1, the published `version_id`. */
export const addedPublicAgent = [
  {
    "id": 4,
    "entity_name": "application",
    "entity_meta": {
      "id": 2,
      "name": "Zefir Travel Planner",
      "project_id": 1
    },
    "entity_settings": {
      "version_id": 2
    },
    "meta": {
      "name": "Zefir Travel Planner",
      "tools": []
    }
  }
];
