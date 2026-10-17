package service

import (
	"context"
	"time"
)

// CleanupResult reports what a retention sweep did.
type CleanupResult struct {
	// Ended counts active sessions marked ended because their CLI went
	// silent past the abandon window.
	Ended int64 `json:"ended"`
	// Deleted counts ended sessions removed after the retention period.
	Deleted int64 `json:"deleted"`
	// Skipped is true when another instance was already sweeping.
	Skipped bool `json:"skipped,omitempty"`
}

// deleteBatch is how many sessions one DELETE removes.
const deleteBatch = 200

// Cleanup runs the retention sweep once. Only one instance sweeps at a
// time.
func (s *Service) Cleanup(ctx context.Context) (CleanupResult, error) {
	var res CleanupResult
	now := s.now()
	ran, err := s.store.WithCleanupLock(ctx, func() error {
		q := s.store.Q()
		var err error
		if res.Ended, err = q.MarkAbandoned(ctx, now.Add(-s.opts.AbandonAfter)); err != nil {
			return err
		}
		res.Deleted, err = q.DeleteExpired(ctx, now.Add(-s.opts.Retention), s.opts.Protected, deleteBatch)
		return err
	})
	res.Skipped = !ran && err == nil
	return res, err
}

// RunCleanup sweeps every interval until ctx is done.
func (s *Service) RunCleanup(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		res, err := s.Cleanup(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			s.log.Error("retention sweep failed", "error", err)
		case res.Ended > 0 || res.Deleted > 0:
			s.log.Info("retention sweep", "ended", res.Ended, "deleted", res.Deleted)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
