// Package billflow is the Temporal workflow that owns the lifecycle of one
// bill. A single workflow execution per bill serialises every add-line-item
// against the close, which is what makes "no line item after close" hold
// without database locks in the request path.
//
// Workflow code here must stay deterministic: no wall clock, randomness, I/O
// or goroutines. Anything of that kind belongs in an activity.
package billflow

import (
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/lmeow01/pave-fees/fees/money"
)

// Update names accepted by the Bill workflow.
const (
	UpdateAddLineItem = "add_line_item"
	UpdateClose       = "close"
)

// Params are fixed for the lifetime of a bill.
type Params struct {
	BillID    string
	Currency  money.Currency
	PeriodEnd time.Time
}

// NewLineItem is the argument of the add-line-item update. The caller picks
// the ID so that the workflow never has to generate one.
type NewLineItem struct {
	ID             string
	Description    string
	Amount         money.Amount
	Currency       money.Currency
	IdempotencyKey string
}

// LineItem is a fee accrued on a bill, as stored.
type LineItem struct {
	ID             string
	BillID         string
	Description    string
	Amount         money.Amount
	Currency       money.Currency
	IdempotencyKey string
	CreatedAt      time.Time
}

type status int

const (
	statusOpen status = iota
	// statusClosing rejects new line items while the ones already accepted
	// finish being written.
	statusClosing
	statusClosed
)

type bill struct {
	params Params
	status status
	// addsInFlight counts accepted line items whose write has not finished.
	addsInFlight int
}

// Bill accrues line items until it is closed, either by the close update or
// when the billing period ends, whichever comes first.
func Bill(ctx workflow.Context, params Params) error {
	b := &bill{params: params}

	err := workflow.SetUpdateHandlerWithOptions(ctx, UpdateAddLineItem, b.addLineItem,
		workflow.UpdateHandlerOptions{Validator: b.validateAddLineItem})
	if err != nil {
		return err
	}
	if err := workflow.SetUpdateHandler(ctx, UpdateClose, b.close); err != nil {
		return err
	}

	untilPeriodEnd := params.PeriodEnd.Sub(workflow.Now(ctx))
	woken, err := workflow.AwaitWithTimeout(ctx, untilPeriodEnd, func() bool {
		return b.status != statusOpen || b.historyNeedsRollover(ctx)
	})
	if err != nil {
		return err
	}
	if woken && b.status == statusOpen {
		return workflow.NewContinueAsNewError(ctx, Bill, params)
	}

	b.status = statusClosing
	if err := workflow.Await(ctx, func() bool { return b.addsInFlight == 0 }); err != nil {
		return err
	}
	var a *Activities
	if err := workflow.ExecuteActivity(withActivityOptions(ctx), a.CloseBill, params.BillID, workflow.Now(ctx)).Get(ctx, nil); err != nil {
		return err
	}
	b.status = statusClosed

	return workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) })
}

// historyNeedsRollover reports whether the event history has grown enough
// that the bill should continue in a fresh execution. Rolling over is only
// safe while no update handler is mid-flight.
func (b *bill) historyNeedsRollover(ctx workflow.Context) bool {
	return workflow.GetInfo(ctx).GetContinueAsNewSuggested() && b.addsInFlight == 0
}

func (b *bill) validateAddLineItem(ctx workflow.Context, item NewLineItem) error {
	// The period can be over before its timer has been processed, for example
	// when the workflow was started late.
	periodEnded := !workflow.Now(ctx).Before(b.params.PeriodEnd)
	if b.status != statusOpen || periodEnded {
		return errBillClosed()
	}
	if item.Currency != b.params.Currency {
		return errCurrencyMismatch(item.Currency, b.params.Currency)
	}
	return nil
}

func (b *bill) addLineItem(ctx workflow.Context, item NewLineItem) (LineItem, error) {
	b.addsInFlight++
	defer func() { b.addsInFlight-- }()

	var a *Activities
	var stored LineItem
	err := workflow.ExecuteActivity(withActivityOptions(ctx), a.InsertLineItem, LineItem{
		ID:             item.ID,
		BillID:         b.params.BillID,
		Description:    item.Description,
		Amount:         item.Amount,
		Currency:       item.Currency,
		IdempotencyKey: item.IdempotencyKey,
		CreatedAt:      workflow.Now(ctx),
	}).Get(ctx, &stored)
	return stored, err
}

func (b *bill) close(ctx workflow.Context) error {
	if b.status == statusOpen {
		b.status = statusClosing
	}
	return workflow.Await(ctx, func() bool { return b.status == statusClosed })
}

func withActivityOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
	})
}
