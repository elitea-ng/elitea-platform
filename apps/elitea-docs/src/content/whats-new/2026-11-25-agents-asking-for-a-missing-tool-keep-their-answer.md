---
date: 2026-11-25
---
## An agent that asks for a tool it does not have no longer ends in an error

When a model, after writing its answer, asked for a tool the agent does not have (for example `gh` or a shell, from instructions written for another setup), the whole reply ended with "Error code: INTERNAL". The answer you already read is now kept: the agent stops there, and a short note at the end names the tool it asked for. If the model asks for such a tool before it has written anything, it is asked once more with only the tools the agent has; if it asks again, the reply ends with "The model returned an incomplete or invalid response" instead of an internal error. Administrators can find the tool name in the worker log: `model_tool_not_admitted_dropped` when the answer was kept, `model_tool_not_admitted` when the model was asked again.
