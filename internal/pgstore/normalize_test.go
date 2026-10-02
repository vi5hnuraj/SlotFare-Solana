package pgstore

import (
	"strings"
	"testing"
)

func TestPG(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`SELECT 1`, `SELECT 1`},
		{`INSERT INTO t (a, b) VALUES (?, ?)`, `INSERT INTO t (a, b) VALUES ($1, $2)`},
		{`UPDATE t SET a = ? WHERE b = ? AND c = ?`, `UPDATE t SET a = $1 WHERE b = $2 AND c = $3`},
		// A multi-line statement, as the shared constants carry it.
		{"SELECT id\n\t FROM usage WHERE account_id = ? AND id > ? ORDER BY id ASC LIMIT 500",
			"SELECT id\n\t FROM usage WHERE account_id = $1 AND id > $2 ORDER BY id ASC LIMIT 500"},
	}
	for _, c := range cases {
		if got := PG(c.in); got != c.want {
			t.Errorf("PG(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPGNumberingIsSequential(t *testing.T) {
	got := PG(strings.Repeat("?, ", 9) + "?")
	want := "$1, $2, $3, $4, $5, $6, $7, $8, $9, $10"
	if got != want {
		t.Errorf("PG of ten placeholders = %q, want %q", got, want)
	}
}
