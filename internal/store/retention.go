package store

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"
)

// deleteOlderThan removes every result whose start time is older than window
// and returns the number of deleted rows.
//
// The cutoff is computed from the current time, so the caller does not need to
// pass one in. The write mutex keeps this delete serialized with Record calls.
// A non-positive window is a misconfiguration and is a no-op: data destruction
// must never be an accident (StartRetention also warns about it).
func (s *Store) deleteOlderThan(window time.Duration) (int64, error) {
	if window <= 0 {
		return 0, nil
	}

	cutoff := time.Now().Add(-window).UnixMilli()

	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`DELETE FROM results WHERE started_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: delete results older than %s: %w", window, err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete results older than %s: %w", window, err)
	}
	return deleted, nil
}

// StartRetention starts a background cleanup loop and returns a channel that is
// closed once the loop has stopped.
//
// One cleanup runs immediately, then one on every period tick. Each cleanup
// removes rows older than window and logs the deleted count at Info level, so a
// healthy retention task produces a periodic line in the service log. A nil
// logger discards that output.
//
// The loop stops when ctx is cancelled: the ticker is stopped before the done
// channel is closed, so callers can wait on done to be sure no cleanup is still
// running. A non-positive period is treated as "run once and stop"; this keeps
// a misconfigured period from panicking in time.NewTicker. A non-positive
// window disables retention entirely with a warning — "delete everything"
// must never be the failure mode of a bad configuration value.
func (s *Store) StartRetention(ctx context.Context, period, window time.Duration, logger *slog.Logger) <-chan struct{} {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	done := make(chan struct{})
	if window <= 0 {
		logger.Warn("retention window is not positive; retention disabled, no rows will be deleted", "window", window)
		close(done)
		return done
	}

	go func() {
		defer close(done)

		cleanup := func() {
			deleted, err := s.deleteOlderThan(window)
			if err != nil {
				// A shutdown racing with Close can surface as an error from the
				// already-closed handle; do not report that as a retention fault.
				if ctx.Err() == nil {
					logger.Error("retention cleanup failed", "window", window, "err", err)
				}
				return
			}
			logger.Info("retention cleanup complete", "deleted", deleted, "window", window)
		}

		cleanup()

		if period <= 0 {
			logger.Warn("retention period is not positive; running a single cleanup", "period", period)
			return
		}

		ticker := time.NewTicker(period)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cleanup()
			}
		}
	}()

	return done
}
