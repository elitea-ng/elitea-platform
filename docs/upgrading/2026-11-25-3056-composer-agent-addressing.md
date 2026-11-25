# Desktop: agents in the composer's `@` menu and the To chip (#3056, ADR-0061)

**What changed.** In a desktop Local work thread with two or more agents, the composer's `@` menu lists the thread's
agents before the folder's files, and a **To:** chip above the composer shows who answers the message being typed
(client-core `firstAnswerer`, the rule Main routes on). A picked agent is plain `@Name` text: the send's
`addressed_to` is computed exactly as before.

**Who is affected.** Desktop users.

**What to do.** No operator action.
