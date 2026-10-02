package pgstore

import (
	"strings"
	"testing"

	"slotfare/internal/funding"
	"slotfare/internal/keys"
	"slotfare/internal/market"
	"slotfare/internal/meter"
	"slotfare/internal/session"
)

// sharedStatements lists every shared constant the Postgres drivers execute,
// with the argument count of the SQLite method that proves it. This is the
// offline proof that the pg statements ARE the sqlite-proven statements:
// each entry must be free of $ (PG rewrites only ?), and its ? count must
// equal the number of arguments the corresponding SQLite method passes.
var sharedStatements = []struct {
	name string
	sql  string
	args int
}{
	{"keys.SQLAdd", keys.SQLAdd, 8},
	{"keys.SQLLookup", keys.SQLLookup, 1},
	{"keys.SQLListByAccount", keys.SQLListByAccount, 1},
	{"keys.SQLRevokeByPrefix", keys.SQLRevokeByPrefix, 2},
	{"keys.SQLTouch", keys.SQLTouch, 2},
	{"meter.SQLLoadAccounts", meter.SQLLoadAccounts, 0},
	{"meter.SQLBalance", meter.SQLBalance, 1},
	{"meter.SQLFlushDelete", meter.SQLFlushDelete, 0},
	{"meter.SQLFlushInsert", meter.SQLFlushInsert, 7},
	{"meter.SQLReserve", meter.SQLReserve, 4},
	{"meter.SQLRelease", meter.SQLRelease, 4},
	{"meter.SQLSettle", meter.SQLSettle, 8}, {"meter.SQLUpsertMinted", meter.SQLUpsertMinted, 5},
	{"meter.SQLRaiseMinted", meter.SQLRaiseMinted, 4},
	{"meter.SQLUpsertLocked", meter.SQLUpsertLocked, 5},
	{"meter.SQLRecordUsageInsert", meter.SQLRecordUsageInsert, 13},
	{"meter.SQLPruneUsage", meter.SQLPruneUsage, 3},
	{"meter.SQLRecentUsageBase", meter.SQLRecentUsageBase, 1},
	{"meter.SQLRecentUsageKeyPrefix", meter.SQLRecentUsageKeyPrefix, 1},
	{"meter.SQLRecentUsageErrorsOnly", meter.SQLRecentUsageErrorsOnly, 1},
	{"meter.SQLRecentUsageBefore", meter.SQLRecentUsageBefore, 1},
	{"meter.SQLRecentUsageTail", meter.SQLRecentUsageTail, 1},
	{"meter.SQLUsagePageBase", meter.SQLUsagePageBase, 1},
	{"meter.SQLUsagePageKeyPrefix", meter.SQLUsagePageKeyPrefix, 1},
	{"meter.SQLUsagePageModel", meter.SQLUsagePageModel, 1},
	{"meter.SQLUsagePageFrom", meter.SQLUsagePageFrom, 1},
	{"meter.SQLUsagePageTo", meter.SQLUsagePageTo, 1},
	{"meter.SQLUsagePageErrorsOnly", meter.SQLUsagePageErrorsOnly, 1},
	{"meter.SQLUsagePageTail", meter.SQLUsagePageTail, 2},
	{"meter.SQLUsageCountBase", meter.SQLUsageCountBase, 1},
	{"meter.SQLUsageFacetsPrefixes", meter.SQLUsageFacetsPrefixes, 1},
	{"meter.SQLUsageFacetsModels", meter.SQLUsageFacetsModels, 1},
	{"meter.SQLUsageSince", meter.SQLUsageSince, 2},
	{"meter.SQLBatchRows", meter.SQLBatchRows, 2},
	{"meter.SQLBatchCursor", meter.SQLBatchCursor, 1},
	{"meter.SQLRecordBatchInsert", meter.SQLRecordBatchInsert, 8},
	{"meter.SQLUsageByID", meter.SQLUsageByID, 2},
	{"meter.SQLBatchForRow", meter.SQLBatchForRow, 3},
	{"meter.SQLBatchRangeRows", meter.SQLBatchRangeRows, 3},
	{"funding.SQLRecordPurchase", funding.SQLRecordPurchase, 17},
	{"funding.SQLOpenAccounts", funding.SQLOpenAccounts, 0},
	{"funding.SQLStampAttested", funding.SQLStampAttested, 3},
	{"funding.SQLMarkCreditedByDeposit", funding.SQLMarkCreditedByDeposit, 7},
	{"funding.SQLRepairDirectPurchase", funding.SQLRepairDirectPurchase, 5}, {"market.SQLRecordListing", market.SQLRecordListing, 13},
	{"market.SQLAccountForListing", market.SQLAccountForListing, 1},
	{"market.SQLCancelListing", market.SQLCancelListing, 4},
	{"market.SQLSoldListing", market.SQLSoldListing, 5},
	{"market.SQLRepairListing", market.SQLRepairListing, 5},
	{"market.SQLSetListingTokenMetadata", market.SQLSetListingTokenMetadata, 3},
	{"market.SQLLockedFor", market.SQLLockedFor, 1},
	{"market.SQLOpenLocks", market.SQLOpenLocks, 0},
	{"funding.SQLCursor", funding.SQLCursor, 1},
	{"funding.SQLSetCursor", funding.SQLSetCursor, 2},
	{"funding.SQLMarkCredited", funding.SQLMarkCredited, 2},
	{"session.SQLSessionSave", session.SQLSessionSave, 5},
	{"session.SQLSessionLookup", session.SQLSessionLookup, 1},
	{"session.SQLSessionTouch", session.SQLSessionTouch, 4},
	{"session.SQLSessionRevoke", session.SQLSessionRevoke, 1},
	{"session.SQLSessionSweep", session.SQLSessionSweep, 1},
	{"session.SQLNonceSave", session.SQLNonceSave, 4},
	{"session.SQLNonceTake", session.SQLNonceTake, 1},
	{"session.SQLNonceSweep", session.SQLNonceSweep, 1},
}

func TestSharedStatementsAreDriverNeutral(t *testing.T) {
	for _, s := range sharedStatements {
		if strings.Contains(s.sql, "$") {
			t.Errorf("%s contains $: a shared constant must stay dialect-neutral", s.name)
		}
		if got := strings.Count(s.sql, "?"); got != s.args {
			t.Errorf("%s has %d placeholders, want %d (the argument count of its SQLite method)", s.name, got, s.args)
		}
	}
}
