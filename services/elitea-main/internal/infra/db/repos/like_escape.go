package repos

import "strings"

// likeEscaper escapes the LIKE/ILIKE metacharacters for a pattern compared
// with `ESCAPE '\'` (Postgres's default escape character). A Replacer never
// re-reads its own output, so the `\` it adds are not escaped again. It is the
// one copy in this package (#1849).
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// escapeLike returns value with `\`, `%` and `_` escaped, so a LIKE or ILIKE
// pattern built around it matches value literally.
func escapeLike(value string) string {
	return likeEscaper.Replace(value)
}
