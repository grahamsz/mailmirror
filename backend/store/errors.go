// File overview: Store-level error helpers.

package store

import (
	"database/sql"
	"errors"
	"fmt"
)

var ErrDuplicateMailboxRole = errors.New("mailbox role already assigned")
var ErrInvalidMailboxSettings = errors.New("invalid mailbox settings")
var ErrInvalidSwipePreferences = errors.New("invalid swipe preferences")
var ErrMailboxGenerationChanged = errors.New("mailbox generation changed")
var ErrMailboxGenerationArrivalUIDFloorRequired = errors.New("mailbox generation reset requires an arrival UID floor")
var ErrOutboxBusy = errors.New("outbox delivery is in progress")

// IsNotFound normalizes sql.ErrNoRows checks across store and web packages.
func IsNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

// WrapNotFound converts sql.ErrNoRows to the store package sentinel used by callers.
func WrapNotFound(thing string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", thing, ErrNotFound)
	}
	return err
}
