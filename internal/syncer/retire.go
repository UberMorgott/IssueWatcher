package syncer

import (
	"context"
	"errors"
)

// ErrRetired means the syncer was removed from its group (its platform was
// switched off or its engine replaced): what it still has in flight stores
// nothing, so a slow request of the old source never overwrites the new one.
var ErrRetired = errors.New("syncer: source removed")

// Retire ends the syncer for good: every operation's context is cancelled,
// a store write in flight finishes, and later writes fail with ErrRetired.
func (s *Syncer) Retire() {
	s.kill()
	s.wmu.Lock()
	s.retired = true
	s.wmu.Unlock()
}

// bind derives ctx that also ends when the syncer is retired.
func (s *Syncer) bind(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.life, cancel)
	return ctx, func() { stop(); cancel() }
}

// write runs f (a store write of synced data) unless the syncer is retired;
// Retire waits for a running f.
func (s *Syncer) write(f func() error) error {
	s.wmu.RLock()
	defer s.wmu.RUnlock()
	if s.retired {
		return ErrRetired
	}
	return f()
}
