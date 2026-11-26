# Web and mobile: read-only team view of a Local work thread (#3069, ADR-0061)

**What changed.** A desktop Local work thread opened on the web or mobile shows its agents with **Local** / **Cloud**
and **Lead**, read-only (web: the chat's participants panel in `local_work` mode; mobile: the info sheet). An answer
whose message `meta.local_work.ran_outside_sandbox` is `true` is marked "Ran outside the sandbox". Main writes that
field from the trust record (#3060); until it does, the field is absent and no marker shows.

**Who is affected.** Web and mobile users.

**What to do.** No operator action.
