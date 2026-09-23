package meter

import (
	"context"
	"time"
)

// Flusher writes the in-memory ledger to the SQLite checkpoint on an
// interval or when total pending crosses a threshold, whichever comes
// first, and once more on shutdown. It is the only component that touches
// SQLite; the request path stays in memory.
type Flusher struct {
	l         *Ledger
	s         Store
	interval  time.Duration
	threshold uint64
}

// NewFlusher returns a Flusher that checkpoints l through s every interval,
// or sooner when total pending reaches threshold. A threshold of zero means
// every tick flushes.
func NewFlusher(l *Ledger, s Store, interval time.Duration, threshold uint64) *Flusher {
	return &Flusher{l: l, s: s, interval: interval, threshold: threshold}
}

// Run blocks until ctx is done. It flushes when total pending reaches the
// threshold - checked on each tick and immediately on any pending
// mutation, whichever comes first - and performs one final flush when ctx
// is cancelled. The shutdown flush uses a fresh context so a cancelled
// caller cannot abort it. Flush errors are reported through onErr (which
// may be nil) and never stop the loop.
func (f *Flusher) Run(ctx context.Context, onErr func(error)) error {
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			f.maybeFlush(ctx, onErr)
		case <-f.l.Changed():
			f.maybeFlush(ctx, onErr)
		case <-ctx.Done():
			f.flush(context.Background(), onErr)
			return nil
		}
	}
}

// maybeFlush writes the checkpoint when total pending has crossed the
// threshold. A context that is already cancelled is skipped: the shutdown
// flush in Run handles the final write.
func (f *Flusher) maybeFlush(ctx context.Context, onErr func(error)) {
	if ctx.Err() != nil {
		return
	}
	if f.pendingTotal() >= f.threshold {
		f.flush(ctx, onErr)
	}
}

// pendingTotal sums the values of the ledger's pending snapshot. It reads
// memory only.
func (f *Flusher) pendingTotal() uint64 {
	var total uint64
	for _, p := range f.l.PendingTotal() {
		total += p
	}
	return total
}

// flush writes one consistent snapshot of the ledger to the store.
func (f *Flusher) flush(ctx context.Context, onErr func(error)) {
	snapshot := f.l.Snapshot()
	if err := f.s.Flush(ctx, snapshot); err != nil && onErr != nil {
		onErr(err)
	}
}
