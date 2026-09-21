package meter

import (
	"context"
)

// Locker single-flights one background loop across replicas. Try attempts
// to take the named lock; when another replica already holds it, ok is
// false and the caller skips the unit silently. release returns the lock
// and the dedicated connection it rode on. A nil or absent locker means no
// gating - today's single-process behavior.
type Locker interface {
	Try(ctx context.Context, key string) (release func(context.Context) error, ok bool, err error)
}
