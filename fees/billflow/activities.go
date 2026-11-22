package billflow

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrBillNotOpen is returned by a Store that refuses to write a line
	// item because the bill is closed or does not exist.
	ErrBillNotOpen = errors.New("bill is not open")
	// ErrPermanent marks a Store failure that no retry can fix, such as
	// data the database refuses. A Store wraps it so the activity fails once
	// rather than retrying forever and holding the bill open.
	ErrPermanent = errors.New("permanent store failure")
)

// Store is the persistence the workflow needs. Both methods are retried by
// Temporal and must be idempotent.
type Store interface {
	// InsertLineItem stores the item and returns it. If an item with the same
	// idempotency key already exists on the bill, that item is returned
	// instead and nothing is written.
	InsertLineItem(ctx context.Context, item LineItem) (LineItem, error)
	// CloseBill marks the bill closed and fixes its total. Closing a closed
	// bill changes nothing.
	CloseBill(ctx context.Context, billID string, closedAt time.Time) error
}

// Activities are the side effects of the Bill workflow.
type Activities struct {
	Store Store
}

// InsertLineItem stores a line item on an open bill.
func (a *Activities) InsertLineItem(ctx context.Context, item LineItem) (LineItem, error) {
	stored, err := a.Store.InsertLineItem(ctx, item)
	if errors.Is(err, ErrBillNotOpen) {
		return LineItem{}, nonRetryableBillClosed(err)
	}
	return stored, failOnceIfPermanent(err)
}

// CloseBill marks the bill closed and fixes its total.
func (a *Activities) CloseBill(ctx context.Context, billID string, closedAt time.Time) error {
	return failOnceIfPermanent(a.Store.CloseBill(ctx, billID, closedAt))
}
