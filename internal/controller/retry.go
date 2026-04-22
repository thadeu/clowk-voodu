package controller

import (
	"errors"
	"time"
)

// TransientError wraps an error that the reconciler should retry later.
// A ref into a database whose plugin hasn't finished provisioning is
// the canonical example: applying a deployment moments before its DB
// is ready shouldn't require a human to re-apply.
//
// Non-transient (i.e. plain) errors are logged and left alone — the
// next watch event for that key is the way out.
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// Transient tags err for retry. Nil in, nil out so call sites can
// stay unbranched (e.g. `return Transient(err)`).
func Transient(err error) error {
	if err == nil {
		return nil
	}

	return &TransientError{Err: err}
}

func isTransient(err error) bool {
	if err == nil {
		return false
	}

	var te *TransientError

	return errors.As(err, &te)
}

// retryBackoff returns the delay before attempt N (1-indexed). Caps at
// 30s so a stuck manifest doesn't starve the retry slot indefinitely.
// Attempt numbers above maxAttempts are not expected to reach this.
func retryBackoff(attempt int) time.Duration {
	switch {
	case attempt <= 1:
		return time.Second
	case attempt == 2:
		return 2 * time.Second
	case attempt == 3:
		return 4 * time.Second
	case attempt == 4:
		return 8 * time.Second
	case attempt == 5:
		return 16 * time.Second
	default:
		return 30 * time.Second
	}
}

// maxRetryAttempts caps transient retries. ~60s of backoff before giving
// up matches the rough "is the DB up yet" window in practice; anything
// longer is almost always a real misconfiguration, not a race.
const maxRetryAttempts = 6
