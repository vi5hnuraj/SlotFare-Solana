package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"slotfare/internal/funding"
	"slotfare/internal/keys"
	"slotfare/internal/market"
	"slotfare/internal/meter"
	"slotfare/internal/pgstore"
	"slotfare/internal/session"
)

// runImportSQLite copies an existing slotfared SQLite database into the
// Postgres named by databaseURL. The source is opened read-only; the target
// must be empty, which Import enforces per table. It prints one line per
// table plus a final parity line.
func runImportSQLite(databaseURL, path string) error {
	if err := validateDatabaseURL(databaseURL); err != nil {
		return err
	}
	src, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open sqlite source %s: %w", path, err)
	}
	defer src.Close()
	if err := src.Ping(); err != nil {
		return fmt.Errorf("open sqlite source %s: %w", path, err)
	}
	ctx := context.Background()
	var version int
	if err := src.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read user_version of %s: %w", path, err)
	}
	if version != 1 {
		return fmt.Errorf("sqlite source %s has user_version %d, want 1: start slotfared once on sqlite so its one-time migrations run, then import", path, version)
	}
	for _, table := range pgstore.PGImportSpec.Tables {
		var exists int
		err := src.QueryRow(`SELECT 1 FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&exists)
		if err == sql.ErrNoRows {
			return fmt.Errorf("sqlite source %s is missing table %s: start slotfared once on sqlite so its schema is created, then import", path, table)
		}
		if err != nil {
			return fmt.Errorf("check table %s in %s: %w", table, path, err)
		}
	}
	// A source written by an old slotfared binary predates the usage.kind
	// column and cannot be copied column-for-column; refuse honestly.
	var kindCol int
	err = src.QueryRow(`SELECT 1 FROM pragma_table_info('usage') WHERE name = 'kind'`).Scan(&kindCol)
	if err == sql.ErrNoRows {
		return fmt.Errorf("sqlite source %s usage table has no kind column: start slotfared once on sqlite so its schema migration runs, then import", path)
	}
	if err != nil {
		return fmt.Errorf("check usage.kind column in %s: %w", path, err)
	}
	dst, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open postgres target: %w", err)
	}
	defer dst.Close()
	counts, err := pgstore.Import(ctx, src, dst, pgstore.PGImportSpec)
	if err != nil {
		return err
	}
	for _, table := range pgstore.PGImportSpec.Tables {
		fmt.Printf("imported %d rows into %s\n", counts[table], table)
	}
	fmt.Println("row counts match")
	return nil
}

// validateDatabaseURL checks that databaseURL is a postgres:// or
// postgresql:// URL, naming the variable in the error when it is not.
func validateDatabaseURL(databaseURL string) error {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return fmt.Errorf("SLOTFARE_DATABASE_URL: parse %q: %w", databaseURL, err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return fmt.Errorf("SLOTFARE_DATABASE_URL %q must use the postgres or postgresql scheme", databaseURL)
	}
	return nil
}

// openStores opens the four stores on the configured backend. When
// storeSet is every store slotfared runs on, opened on one backend.
// Loading and closing them together keeps the backend decision (and its
// cleanup) in one place instead of spread across the daemon's wiring.
type storeSet struct {
	keys    keys.Store
	meter   meter.Store
	funding funding.Store
	market  market.Store
	session session.Store
	label   string
	atomic  meter.AtomicLedger
	locker  meter.Locker
	// sharedMarket records that funding and market are one object (the
	// Postgres driver serves both interfaces), so Close closes it once.
	sharedMarket bool
}

// Close releases every store exactly once: sessions first, then the stores
// whose pools the others share, then the meter store and the key store. A
// nil set (openStores failed before it returned) closes nothing.
func (s *storeSet) Close() {
	if s == nil {
		return
	}
	if s.session != nil {
		s.session.Close()
	}
	if s.market != nil && !s.sharedMarket {
		s.market.Close()
	}
	if s.funding != nil {
		s.funding.Close()
	}
	if s.meter != nil {
		s.meter.Close()
	}
	if s.keys != nil {
		s.keys.Close()
	}
}

// openStores opens the stores on the configured backend. When databaseURL
// is set it opens Postgres stores (SLOTFARE_DB is ignored); otherwise it
// opens the SQLite stores at dbPath, exactly as before. The returned label
// names the active backend for the startup log line. The atomic authority
// and locker are non-nil only in Postgres mode: the balance authority is
// the shared accounts table, and the loops single-fly through advisory
// locks on the meter store's pool.
func openStores(ctx context.Context, databaseURL, dbPath string, sourceChainKey uint64) (*storeSet, error) {
	if databaseURL != "" {
		if err := validateDatabaseURL(databaseURL); err != nil {
			return nil, err
		}
		s := &storeSet{label: "postgres"}
		var err error
		if s.keys, err = pgstore.NewKeysStore(ctx, databaseURL); err != nil {
			return nil, err
		}
		meterStore, err := pgstore.NewMeterStore(ctx, databaseURL)
		if err != nil {
			s.keys.Close()
			return nil, err
		}
		s.meter = meterStore
		// NewMeterStore always returns a *pgstore.MeterStore, which is the
		// balance authority and carries the pool the locker rides on.
		pgMeter := meterStore.(*pgstore.MeterStore)
		s.atomic = pgMeter
		// The locker rides the same pool: an advisory lock is a session
		// attribute, and the pool's sessions outlive any single query.
		s.locker = pgstore.NewAdvisoryLocker(pgMeter.DB())
		pgFunding, err := pgstore.NewFundingStore(ctx, databaseURL, sourceChainKey)
		if err != nil {
			s.keys.Close()
			s.meter.Close()
			return nil, err
		}
		// One Postgres object serves the purchase and the listing
		// interfaces alike, so the set closes it through funding alone.
		s.funding = pgFunding
		s.market = pgFunding
		s.sharedMarket = true
		if s.session, err = pgstore.NewSessionsStore(ctx, databaseURL); err != nil {
			s.keys.Close()
			s.meter.Close()
			s.funding.Close()
			return nil, err
		}
		return s, nil
	}
	s := &storeSet{label: "sqlite " + dbPath}
	var err error
	if s.keys, err = keys.Open(dbPath); err != nil {
		return nil, err
	}
	if s.meter, err = meter.Open(dbPath); err != nil {
		s.keys.Close()
		return nil, err
	}
	if s.funding, err = funding.NewStore(dbPath, sourceChainKey); err != nil {
		s.Close()
		return nil, err
	}
	if s.market, err = market.NewStore(dbPath); err != nil {
		s.Close()
		return nil, err
	}
	if s.session, err = session.NewStore(dbPath); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
