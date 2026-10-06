package main

import (
	"flag"
	"os"
	"testing"
)

func TestValidateDatabaseURL(t *testing.T) {
	cases := []struct {
		url  string
		ok   bool
		frag string // substring the error must carry when !ok
	}{
		{"postgres://user:pass@127.0.0.1:5439/slotfared", true, ""},
		{"postgresql://user:pass@db:5432/slotfared", true, ""},
		{"http://127.0.0.1:5439/slotfared", false, "postgres"},
		{"sqlite:///slotfared.db", false, "scheme"},
	}
	for _, c := range cases {
		err := validateDatabaseURL(c.url)
		if c.ok && err != nil {
			t.Errorf("validateDatabaseURL(%q) = %v, want nil", c.url, err)
		}
		if !c.ok && err == nil {
			t.Errorf("validateDatabaseURL(%q) = nil, want an error", c.url)
			continue
		}
		if !c.ok && c.frag != "" && !contains(err.Error(), c.frag) {
			t.Errorf("validateDatabaseURL(%q) error %q does not name %q", c.url, err, c.frag)
		}
	}
}

// TestImportSQLiteSubcommandDispatch checks that the first-argument dispatch
// treats import-sqlite as a subcommand and everything else as the service's
// flag set, exactly as today.
func TestImportSQLiteSubcommandDispatch(t *testing.T) {
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	t.Run("import-sqlite takes its own flag set", func(t *testing.T) {
		os.Args = []string{"slotfared", "import-sqlite", "-database-url", "postgres://x", "slotfared.db"}
		if os.Args[1] != "import-sqlite" {
			t.Fatal("test setup")
		}
		dbURL := ""
		fs := flag.NewFlagSet("slotfared import-sqlite", flag.ContinueOnError)
		fs.StringVar(&dbURL, "database-url", "", "")
		if err := fs.Parse(os.Args[2:]); err != nil {
			t.Fatalf("subcommand flag parse: %v", err)
		}
		if fs.NArg() != 1 || fs.Arg(0) != "slotfared.db" {
			t.Fatalf("subcommand args = %v, want [slotfared.db]", fs.Args())
		}
		if dbURL != "postgres://x" {
			t.Errorf("dbURL = %q, want postgres://x", dbURL)
		}
	})
	t.Run("unknown first arg still reaches the service flag parser", func(t *testing.T) {
		os.Args = []string{"slotfared", "-listen", "127.0.0.1:9999"}
		if os.Args[1] == "import-sqlite" {
			t.Fatal("test setup")
		}
		fs := flag.NewFlagSet("slotfared", flag.ContinueOnError)
		var listen string
		fs.StringVar(&listen, "listen", "", "")
		if err := fs.Parse(os.Args[1:]); err != nil {
			t.Fatalf("service flag parse failed for a non-subcommand arg: %v", err)
		}
		if listen != "127.0.0.1:9999" {
			t.Errorf("listen = %q, want 127.0.0.1:9999", listen)
		}
	})
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
