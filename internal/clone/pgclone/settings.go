package pgclone

import (
	"context"
	"fmt"
	"strings"

	"github.com/mickamy/pgswitch/internal/pgadmin"
	"github.com/mickamy/pgswitch/internal/pgadmin/quote"
)

// listQuoted holds the settings whose stored value is a list of quoted
// elements, as pg_dump's variable_is_guc_list_quote does. Their elements are
// re-quoted one by one; quoting the whole value would make it one element.
var listQuoted = map[string]bool{
	"local_preload_libraries":   true,
	"search_path":               true,
	"session_preload_libraries": true,
	"shared_preload_libraries":  true,
	"temp_tablespaces":          true,
	"unix_socket_directories":   true,
}

// copySettings replays source's ALTER DATABASE ... SET and ALTER ROLE ... IN
// DATABASE ... SET entries on staging, with the privileges those commands need
// rather than a write to the catalog.
func copySettings(ctx context.Context, conn pgadmin.Conn, source, staging string) error {
	rows, err := conn.Query(ctx, "select case when s.setrole = 0 then '' else pg_get_userbyid(s.setrole) end,"+
		" unnest(s.setconfig)"+
		" from pg_db_role_setting s join pg_database d on d.oid = s.setdatabase"+
		" where d.datname = "+quote.Literal(source))
	if err != nil {
		return fmt.Errorf("pgclone: read settings of %s: %w", source, err)
	}

	for _, row := range rows {
		role, entry := row[0].String, row[1].String
		stmt, err := settingStatement(staging, role, entry)
		if err != nil {
			return err
		}
		if err := conn.Exec(ctx, stmt); err != nil {
			scope := "the database"
			if role != "" {
				scope = "role " + role
			}
			name, _, _ := strings.Cut(entry, "=")

			return fmt.Errorf("pgclone: copy setting %s for %s of %s: %w", name, scope, source, err)
		}
	}

	return nil
}

// settingStatement turns a stored "name=value" entry into the command that sets
// it on db, for role or, when role is empty, for everyone.
func settingStatement(db, role, entry string) (string, error) {
	name, value, ok := strings.Cut(entry, "=")
	if !ok || name == "" {
		return "", fmt.Errorf("pgclone: malformed setting %q", entry)
	}

	scope := "alter database " + quote.Ident(db)
	if role != "" {
		scope = "alter role " + quote.Ident(role) + " in database " + quote.Ident(db)
	}

	return scope + " set " + settingName(name) + " = " + settingValue(name, value), nil
}

// settingName quotes each part of a possibly qualified name such as
// myapp.tenant.
func settingName(name string) string {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = quote.Ident(p)
	}

	return strings.Join(parts, ".")
}

func settingValue(name, value string) string {
	if !listQuoted[strings.ToLower(name)] {
		return quote.Literal(value)
	}

	elems := splitList(value)
	if len(elems) == 0 {
		return "''"
	}
	for i, e := range elems {
		elems[i] = quote.Ident(e)
	}

	return strings.Join(elems, ", ")
}

// splitList splits a stored list value such as `"$user", public` into its
// elements. A quoted element may contain commas and doubled quotes.
func splitList(value string) []string {
	var elems []string
	rest := strings.TrimSpace(value)
	for rest != "" {
		var elem string
		if rest[0] == '"' {
			elem, rest = cutQuoted(rest[1:])
			_, rest, _ = strings.Cut(rest, ",")
		} else {
			elem, rest, _ = strings.Cut(rest, ",")
			elem = strings.TrimSpace(elem)
		}
		elems = append(elems, elem)
		rest = strings.TrimSpace(rest)
	}

	return elems
}

// cutQuoted reads a double-quoted element whose opening quote is already gone.
func cutQuoted(s string) (elem, rest string) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '"' {
			b.WriteByte(s[i])

			continue
		}
		if i+1 < len(s) && s[i+1] == '"' {
			b.WriteByte('"')
			i++

			continue
		}

		return b.String(), s[i+1:]
	}

	return b.String(), ""
}
