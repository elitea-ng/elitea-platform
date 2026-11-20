---
date: 2026-11-13
order: 0
---
## Length limits on names and descriptions

A name (agent, pipeline, toolkit, credential, personal access token, folder, conversation, skill) is now at most 128 characters and a description at most 2000. The forms in the web, desktop and mobile apps show a character counter near the limit and block Save past it; the API answers 400 with the field and the limit. **Run the migrations:** stored text over the limits is truncated. See [Create a personal access token](/getting-started/create-personal-access-token#name-limit).
