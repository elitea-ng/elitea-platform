/**
 * Client contract 1.3 chat shapes, CAPTURED 2026-10-06 from the zefir-wave
 * stack (elitea-platform main 22403f640, Python worker, mock LLM echoing the
 * prompt as `MOCK: …`) as Alice (user 8) in her personal project 90501. The
 * bodies are verbatim; only the variable names are ours. The 1.3 branch
 * (507a13b0c) tags these routes `client` without changing their bodies.
 */

export const CAPTURE = {
  "projectId": 90501,
  "conversationId": "3",
  "conversationUuid": "9bb96505-d0fb-4473-be67-4884de5047d3",
  "agentParticipantId": 3,
  "questionUuid": "36f70ef8-6009-411e-b4e3-ad99917eb63b",
  "answerUuid": "0661c34b-1930-5cfe-9dca-c204d0ed0047"
} as const;

/** The conversation's participants (getConversation). */
export const participants13 = [
  {
    "id": 1,
    "entity_name": "user",
    "entity_meta": {
      "id": 8
    },
    "entity_settings": {},
    "meta": {
      "user_name": "Alice Zefir"
    }
  },
  {
    "id": 2,
    "entity_name": "dummy",
    "entity_meta": {},
    "entity_settings": {},
    "meta": {
      "name": "EliteA"
    }
  },
  {
    "id": 3,
    "entity_name": "application",
    "entity_meta": {
      "id": "1",
      "name": "Zefir Tool Agent",
      "project_id": 90501
    },
    "entity_settings": {
      "version_id": 1
    },
    "meta": {
      "name": "Zefir Tool Agent",
      "tools": [
        {
          "id": 1,
          "name": "Zefir Mock Tools personal",
          "selected_tools": [],
          "toolkit_name": "Zefir Mock Tools personal",
          "type": "openapi"
        }
      ],
      "version_status": "draft",
      "version_withdrawn": false
    }
  }
];

/** The question group, and the answer before and after a regeneration (getConversation, `sort_order=desc`). */
export const questionGroup = {
  "author_participant_id": 1,
  "content": "Show me code:\n```python\nprint('hi')\n```\nthanks",
  "created_at": "2026-10-06 20:58:51.243422",
  "id": 5,
  "is_streaming": false,
  "message_items": [
    {
      "id": 5,
      "item_details": {
        "content": "Show me code:\n```python\nprint('hi')\n```\nthanks"
      },
      "item_type": "text_message",
      "order_index": 0
    }
  ],
  "meta": {},
  "sent_to_id": 3,
  "uuid": "36f70ef8-6009-411e-b4e3-ad99917eb63b"
};
export const answerGroupBefore = {
  "author_participant_id": 3,
  "content": "MOCK: Show me code:\n```python\nprint('hi')\n```\nthanks ",
  "created_at": "2026-10-06 20:58:52.243555",
  "id": 6,
  "is_streaming": false,
  "message_items": [
    {
      "id": 7,
      "item_details": {
        "content": "MOCK: Show me code:\n```python\nprint('hi')\n```\nthanks "
      },
      "item_type": "text_message",
      "order_index": 0
    }
  ],
  "meta": {
    "error": "",
    "execution_generation": "36f70ef8-6009-411e-b4e3-ad99917eb63b",
    "invoked_skills": [],
    "is_error": false,
    "memories_used": 2,
    "references": [],
    "thread_id": "9bb96505-d0fb-4473-be67-4884de5047d3"
  },
  "reply_to_id": 5,
  "task_id": "c2b455b08f2dd4e9c7c6232c9b9c6a0c",
  "updated_at": "2026-10-06 20:58:53.608451",
  "uuid": "0661c34b-1930-5cfe-9dca-c204d0ed0047"
};
/** After regenerateChatMessage: same uuid, a new task_id and item, `meta.execution_generation` = the regeneration_id. */
export const answerGroupAfterRegenerate = {
  "author_participant_id": 3,
  "content": "MOCK: Show me code:\n```python\nprint('hi')\n```\nthanks ",
  "created_at": "2026-10-06 20:58:52.243555",
  "id": 6,
  "is_streaming": false,
  "message_items": [
    {
      "id": 13,
      "item_details": {
        "content": "MOCK: Show me code:\n```python\nprint('hi')\n```\nthanks "
      },
      "item_type": "text_message",
      "order_index": 0
    }
  ],
  "meta": {
    "error": "",
    "execution_generation": "f59416e1-c2ef-436a-8108-674c616d3dcf",
    "invoked_skills": [],
    "is_error": false,
    "memories_used": 2,
    "references": [],
    "thread_id": "9bb96505-d0fb-4473-be67-4884de5047d3"
  },
  "reply_to_id": 5,
  "task_id": "dda3ca511b5c6509eec676daf7fd529d",
  "updated_at": "2026-10-06 21:00:26.740144",
  "uuid": "0661c34b-1930-5cfe-9dca-c204d0ed0047"
};
export const REGENERATION_ID = "f59416e1-c2ef-436a-8108-674c616d3dcf";

/** regenerateChatMessage 200 (the stream is __fixtures__/regenerate-answer.sse). */
export const regenerateStarted = {
  "command_id": "28f49c70810cfc91959b2f99c064d655",
  "created": true,
  "events_url": "/api/v2/executions/90501/dda3ca511b5c6509eec676daf7fd529d/events",
  "execution_id": "dda3ca511b5c6509eec676daf7fd529d",
  "response_message_id": "0661c34b-1930-5cfe-9dca-c204d0ed0047",
  "task_id": "dda3ca511b5c6509eec676daf7fd529d"
};
/** regenerateChatMessage 409 while the previous answer is still being finalised (Retry-After: 1). */
export const regeneratePending = {
  "error": "agent_regeneration_pending",
  "message": "The previous agent response is still being finalized. Please retry shortly.",
  "retryable": true
};

/** Message feedback (getMessageFeedback / setMessageFeedback / deleteMessageFeedback). */
export const feedbackEmpty = {
  "likes": 0,
  "dislikes": 0
};
export const feedbackDisliked = {
  "likes": 0,
  "dislikes": 1,
  "mine": {
    "rating": -1,
    "comment": "too short"
  }
};
export const feedbackLiked = {
  "likes": 1,
  "dislikes": 0,
  "mine": {
    "rating": 1
  }
};
export const feedbackBadRating = {
  "error": "rating must be 1 (like) or -1 (dislike)"
};

/** exportConversation `format=md` (200, text/markdown) and an unsupported format (400). */
export const exportMarkdown = "# Quick wins capture\n\n- Conversation: 3\n- Project: 90501\n- Messages: 2\n- Exported: 2026-10-06T20:58:55Z\n\n---\n\n## Alice Zefir — 2026-10-06T20:58:51Z\n\nShow me code:\n```python\nprint('hi')\n```\nthanks\n\n---\n\n## Zefir Tool Agent — 2026-10-06T20:58:52Z\n\nMOCK: Show me code:\n```python\nprint('hi')\n```\nthanks \n\n";
export const exportHeaders = {
  "Content-Disposition": "attachment; filename=Quick_wins_capture.md",
  "Content-Type": "text/markdown; charset=utf-8"
};
export const exportBadFormat = {
  "error": "unsupported export format: use md or json"
};

/** getRecommendations in the personal project and in the team project 90504. */
export const recommendationsPersonal = {
  "applications": [
    {
      "description": "Calls the mock tool; every call needs your approval (HITL).",
      "id": "1",
      "likes": 0,
      "name": "Zefir Tool Agent"
    }
  ],
  "total": 1
};
export const recommendationsTeam = {
  "applications": [
    {
      "description": "Friendly team helper agent.",
      "id": "2",
      "likes": 0,
      "name": "Zefir Helper"
    },
    {
      "description": "Calls the mock tool; every call needs your approval (HITL).",
      "id": "1",
      "likes": 0,
      "name": "Zefir Ops Agent"
    },
    {
      "description": "Zefir Concierge: a public agent anyone can find in Discover.",
      "id": "5",
      "likes": 0,
      "name": "Zefir Concierge"
    }
  ],
  "total": 3
};

/** cancelChatExecution: 204 (no body) for a running answer and again for the same caller; 409 otherwise. */
export const cancelNotActive = {
  "error": "Message is not active or cannot be stopped by this user"
};

/** getConversation (`messages_limit=50&sort_order=desc`) after the first answer: the question and the answer with a code block. */
export const conversationDetail13 = {
  "created_at": "2026-10-06T20:58:51.18841Z",
  "created_by": "8",
  "description": "",
  "folder_id": null,
  "id": "3",
  "instructions": "",
  "is_private": true,
  "message_count": 2,
  "message_groups": [
    {
      "author_participant_id": 3,
      "content": "MOCK: Show me code:\n```python\nprint('hi')\n```\nthanks ",
      "created_at": "2026-10-06 20:58:52.243555",
      "id": 6,
      "is_streaming": false,
      "message_items": [
        {
          "id": 7,
          "item_details": {
            "content": "MOCK: Show me code:\n```python\nprint('hi')\n```\nthanks "
          },
          "item_type": "text_message",
          "order_index": 0
        }
      ],
      "meta": {
        "error": "",
        "execution_generation": "36f70ef8-6009-411e-b4e3-ad99917eb63b",
        "invoked_skills": [],
        "is_error": false,
        "memories_used": 2,
        "references": [],
        "thread_id": "9bb96505-d0fb-4473-be67-4884de5047d3"
      },
      "reply_to_id": 5,
      "task_id": "c2b455b08f2dd4e9c7c6232c9b9c6a0c",
      "updated_at": "2026-10-06 20:58:53.608451",
      "uuid": "0661c34b-1930-5cfe-9dca-c204d0ed0047"
    },
    {
      "author_participant_id": 1,
      "content": "Show me code:\n```python\nprint('hi')\n```\nthanks",
      "created_at": "2026-10-06 20:58:51.243422",
      "id": 5,
      "is_streaming": false,
      "message_items": [
        {
          "id": 5,
          "item_details": {
            "content": "Show me code:\n```python\nprint('hi')\n```\nthanks"
          },
          "item_type": "text_message",
          "order_index": 0
        }
      ],
      "meta": {},
      "sent_to_id": 3,
      "uuid": "36f70ef8-6009-411e-b4e3-ad99917eb63b"
    }
  ],
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
    },
    "single_participant": {
      "entity_meta": {
        "id": 1,
        "name": "Zefir Tool Agent",
        "project_id": 90501
      },
      "entity_name": "application",
      "entity_settings": {
        "version_id": 1
      }
    }
  },
  "name": "Quick wins capture",
  "participants": [
    {
      "id": 1,
      "entity_name": "user",
      "entity_meta": {
        "id": 8
      },
      "entity_settings": {},
      "meta": {
        "user_name": "Alice Zefir"
      }
    },
    {
      "id": 2,
      "entity_name": "dummy",
      "entity_meta": {},
      "entity_settings": {},
      "meta": {
        "name": "EliteA"
      }
    },
    {
      "id": 3,
      "entity_name": "application",
      "entity_meta": {
        "id": "1",
        "name": "Zefir Tool Agent",
        "project_id": 90501
      },
      "entity_settings": {
        "version_id": 1
      },
      "meta": {
        "name": "Zefir Tool Agent",
        "tools": [
          {
            "id": 1,
            "name": "Zefir Mock Tools personal",
            "selected_tools": [],
            "toolkit_name": "Zefir Mock Tools personal",
            "type": "openapi"
          }
        ],
        "version_status": "draft",
        "version_withdrawn": false
      }
    }
  ],
  "project_id": "90501",
  "source": "elitea",
  "updated_at": "2026-10-06T20:58:51.239141Z",
  "uuid": "9bb96505-d0fb-4473-be67-4884de5047d3"
};
