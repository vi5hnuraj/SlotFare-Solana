package pgstore

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"

	"slotfare/internal/funding"
	"slotfare/internal/keys"
	"slotfare/internal/market"
	"slotfare/internal/meter"
	"slotfare/internal/session"
)

// openPool opens one Postgres connection pool for dsn and applies the
// idempotent migration. Each store opens its own pool, mirroring how the
// SQLite driver opens the shared file three times.
func openPool(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("pgstore: open %s: %w", dsn, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("pgstore: connect: %w", err)
	}
	if err := Migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// NewKeysStore returns the Postgres keys.Store for dsn.
func NewKeysStore(ctx context.Context, dsn string) (keys.Store, error) {
	db, err := openPool(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &KeysStore{db: db}, nil
}

// NewMeterStore returns the Postgres meter.Store for dsn.
func NewMeterStore(ctx context.Context, dsn string) (meter.Store, error) {
	db, err := openPool(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &MeterStore{db: db, qf: PG}, nil
}

// NewFundingStore returns the Postgres funding store for dsn. The returned
// value satisfies both funding.Store and market.Store: one object backs the
// purchase and the listing ports on Postgres.
// sourceChainKey is unused by the Postgres schema - there are no legacy
// rows to stamp with it - but kept in the signature so the two drivers
// read the same way at the call site.
func NewFundingStore(ctx context.Context, dsn string, sourceChainKey uint64) (*FundingStore, error) {
	db, err := openPool(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &FundingStore{db: db}, nil
}

var (
	_ keys.Store    = (*KeysStore)(nil)
	_ meter.Store   = (*MeterStore)(nil)
	_ funding.Store = (*FundingStore)(nil)
	_ market.Store  = (*FundingStore)(nil)
	_ session.Store = (*SessionsStore)(nil)
)
