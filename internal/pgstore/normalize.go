// Package pgstore is slotfared's Postgres storage backend. It implements the
// store interfaces of internal/keys, internal/meter and internal/console
// against a Postgres database, selected by SLOTFARE_DATABASE_URL. The SQL it
// executes is the same dialect-neutral text the SQLite drivers use, moved to
// shared constants in those packages; PG rewrites each ? placeholder to a
// sequential $n for the pgx driver. The only intentional divergence from
// the SQLite statements is ORDER BY rowid, which has no Postgres
// equivalent: the serial id columns added here mirror the implicit rowid
// insert order instead.
package pgstore

import (
	"strconv"
	"strings"
)

// PG rewrites each ? placeholder in q to $1, $2, ... in order of
// appearance. The shared constants carry no quoted strings that contain a
// ?, so a plain scan is safe; the shared-SQL guard test asserts this.
func PG(q string) string {
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
