package quote

import "strings"

// Ident quotes an identifier such as a database name.
func Ident(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Literal quotes a string literal. It assumes standard_conforming_strings,
// the default since Postgres 9.1, under which backslashes are not special.
func Literal(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
