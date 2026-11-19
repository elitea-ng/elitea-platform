# Bitbucket: `delete_file` and the per-call repository (issue 2265)

**What changed.** The Bitbucket toolkit gained the SDK's `delete_file` tool (category `delete`, so approval
policies that gate destructive tools apply to it) and every Bitbucket tool now accepts the SDK's per-call
`project` and `repository` arguments, so one toolkit can act on other repositories of the same Bitbucket
host with the same credential. The toolkit editor lists `delete_file` for the `bitbucket` type.

**Who is affected.** Agents and pipelines that use a Bitbucket toolkit. Nothing is removed and no saved
selection changes. `delete_file` works on Bitbucket Cloud; Bitbucket Server has no file-deletion endpoint,
and the tool says so instead of acting, as the SDK does.

**What to do.** Nothing. Operators who want agents not to delete files already have the lever: the
`delete` category in Runtime safety, or leaving `delete_file` out of the toolkit's tool selection. Roll the
Worker together with Main as usual: the editor lists the tool from Main's catalogue.
