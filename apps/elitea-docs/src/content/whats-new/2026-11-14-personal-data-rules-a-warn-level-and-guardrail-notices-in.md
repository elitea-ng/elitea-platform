---
date: 2026-11-14
order: 0
---
## Personal data rules, a warn level, and guardrail notices in the chat

The LLM gateway has a built-in **personal data** detector next to the secret rules: `personal_data` finds e-mail addresses, phone numbers, payment cards (Luhn-checked) and IBANs (mod-97-checked), with no service to run. A guardrail detector has a new action, **warn**, which changes nothing and tells the person. **Admin › LLM Governance › Credentials / Personal data** creates the matching entries on one screen. **When a guardrail blocks, redacts or warns about a message, the chat now says so**, on web, desktop and mobile, with the name of the entry and the kind of data found, never the data. A blocked message no longer shows a generic runtime error. A message is still stored as the person wrote it: only what goes to the model is masked. See [Content controls](/admin/enterprise-controls/content-controls#chat-notices).
