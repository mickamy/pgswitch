package quote

import "strings"

// Ident quotes an identifier such as a database name.
func Ident(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Literal quotes a string literal. It uses the E'...' form, whose backslash
// escapes do not depend on standard_conforming_strings, as quote_literal does.
// The connection must use a client_encoding such as UTF8 where a backslash byte
// never ends a multibyte character.
func Literal(s string) string {
	return "E'" + strings.NewReplacer(`\`, `\\`, "'", "''").Replace(s) + "'"
}
