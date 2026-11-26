# Desktop: the Participants tab of a Local work thread (#3055, ADR-0061)

**What changed.** A desktop Local work thread with agents gets a **Participants** tab in the inspector (inspector tab ids
`team`, first with two or more agents, and `participants`, after the others with one). It reuses the chat's
participants panel in a `local_work` appearance (agents only). Its writes are the chat's existing ones: the lead
(`meta.floor`), where an agent runs (`entity_settings.executor`) and the participant delete.

**Who is affected.** Desktop users. A stored inspector layout gains the tab when it is first offered; nothing else
moves.

**What to do.** No operator action. No migration, no configuration, no new API.
