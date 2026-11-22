package fees

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"encore.dev/beta/errs"
	"github.com/google/uuid"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/lmeow01/pave-fees/fees/billflow"
	"github.com/lmeow01/pave-fees/fees/money"
)

// Bill is the API representation of a bill. Amounts are strings of minor
// units: "1050" in USD is $10.50.
type Bill struct {
	ID         string     `json:"id"`
	CustomerID string     `json:"customer_id"`
	Currency   string     `json:"currency"`
	Status     string     `json:"status"`
	PeriodEnd  time.Time  `json:"period_end"`
	CreatedAt  time.Time  `json:"created_at"`
	ClosedAt   *time.Time `json:"closed_at"`
	// TotalAmount is final once the bill is closed. On an open bill it is
	// the sum accrued so far.
	TotalAmount string `json:"total_amount"`
	// LineItems is the first page of the bill's line items; the rest are
	// read through ListLineItems. It is omitted when listing bills.
	LineItems *LineItemList `json:"line_items,omitempty"`

	HTTPStatus int `json:"-" encore:"httpstatus"`
}

// CreateBillParams is the request of CreateBill.
type CreateBillParams struct {
	IdempotencyKey string `header:"Idempotency-Key"`

	CustomerID string `json:"customer_id"`
	Currency   string `json:"currency"`
	// PeriodEnd is when the bill closes itself if nobody closes it earlier.
	PeriodEnd time.Time `json:"period_end"`
}

// CreateBill opens a bill for a billing period and starts the workflow that
// owns it. Retrying with the same Idempotency-Key returns the same bill.
//
//encore:api public method=POST path=/v1/bills
func (s *Service) CreateBill(ctx context.Context, p *CreateBillParams) (*Bill, error) {
	if err := validateText("Idempotency-Key header", p.IdempotencyKey, maxKeyLength); err != nil {
		return nil, err
	}
	if err := validateText("customer_id", p.CustomerID, maxKeyLength); err != nil {
		return nil, err
	}
	currency, err := money.ParseCurrency(p.Currency)
	if err != nil {
		return nil, invalidArgument(err.Error())
	}
	// Postgres keeps microseconds; truncating up front keeps a retried
	// request comparable with what was stored.
	periodEnd := p.PeriodEnd.Truncate(time.Microsecond)

	b, replayed, err := s.store.billByKey(ctx, p.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	if !replayed {
		if !periodEnd.After(time.Now()) {
			return nil, invalidArgument("period_end must be in the future")
		}
		b, err = s.store.createBill(ctx, newBill{
			ID:             newID(billIDPrefix),
			CustomerID:     p.CustomerID,
			Currency:       currency,
			PeriodEnd:      periodEnd,
			IdempotencyKey: p.IdempotencyKey,
		})
		if err != nil {
			return nil, err
		}
	}
	if b.CustomerID != p.CustomerID || b.Currency != currency || !b.PeriodEnd.Equal(periodEnd) {
		return nil, errIdempotencyKeyReused()
	}

	if b.Status == statusOpen {
		if err := s.startBillWorkflow(ctx, b); err != nil {
			return nil, errs.WrapCode(err, errs.Unavailable, "start bill workflow")
		}
	}

	resp, err := s.billResponse(ctx, b)
	if err != nil {
		return nil, err
	}
	resp.HTTPStatus = http.StatusCreated
	return resp, nil
}

// startBillWorkflow is safe to repeat for the same open bill: a workflow that
// is already running is reused.
func (s *Service) startBillWorkflow(ctx context.Context, b bill) error {
	_, err := s.temporal.ExecuteWorkflow(ctx, billWorkflowOptions(b), billflow.Bill, billWorkflowParams(b))
	return err
}

// updateBill sends an update to the bill's workflow, starting the workflow
// first if it is not running. That repairs a bill whose workflow was never
// started (CreateBill failed between its two steps and was not retried) or
// was terminated.
func (s *Service) updateBill(ctx context.Context, b bill, update client.UpdateWorkflowOptions, result any) error {
	update.WorkflowID = b.ID
	update.WaitForStage = client.WorkflowUpdateStageCompleted
	handle, err := s.temporal.UpdateWithStartWorkflow(ctx, client.UpdateWithStartWorkflowOptions{
		StartWorkflowOperation: s.temporal.NewWithStartWorkflowOperation(
			billWorkflowOptions(b), billflow.Bill, billWorkflowParams(b)),
		UpdateOptions: update,
	})
	if err != nil {
		return err
	}
	return handle.Get(ctx, result)
}

// billWorkflowOptions allow a new run only after a failed or terminated one,
// so no second workflow starts for a bill whose workflow completed.
func billWorkflowOptions(b bill) client.StartWorkflowOptions {
	return client.StartWorkflowOptions{
		ID:                       b.ID,
		TaskQueue:                taskQueue,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	}
}

func billWorkflowParams(b bill) billflow.Params {
	return billflow.Params{BillID: b.ID, Currency: b.Currency, PeriodEnd: b.PeriodEnd}
}

// ListBillsParams is the request of ListBills.
type ListBillsParams struct {
	CustomerID string `query:"customer_id"`
	// Status is "open" or "closed"; empty lists both.
	Status        string `query:"status"`
	PageSize      int    `query:"page_size"`
	NextPageToken string `query:"next_page_token"`
}

// BillList is one page of bills.
type BillList struct {
	Data []Bill `json:"data"`
	// NextPageToken is empty on the last page.
	NextPageToken string `json:"next_page_token"`
}

// ListBills returns a customer's bills, newest first, without their line
// items.
//
//encore:api public method=GET path=/v1/bills
func (s *Service) ListBills(ctx context.Context, p *ListBillsParams) (*BillList, error) {
	if err := validateText("customer_id", p.CustomerID, maxKeyLength); err != nil {
		return nil, err
	}
	if p.Status != "" && p.Status != statusOpen && p.Status != statusClosed {
		return nil, invalidArgument(`status must be "open" or "closed"`)
	}
	if err := validatePageToken(p.NextPageToken, billIDPrefix); err != nil {
		return nil, err
	}
	size, err := pageSize(p.PageSize)
	if err != nil {
		return nil, err
	}

	bills, err := s.store.bills(ctx, billsFilter{
		CustomerID: p.CustomerID,
		Status:     p.Status,
		Before:     p.NextPageToken,
	}, size+1)
	if err != nil {
		return nil, err
	}
	page, next := splitPage(bills, size, func(b bill) string { return b.ID })

	resp := &BillList{Data: make([]Bill, len(page)), NextPageToken: next}
	for i, b := range page {
		resp.Data[i] = billSummary(b)
	}
	return resp, nil
}

// GetBill returns a bill with its line items.
//
//encore:api public method=GET path=/v1/bills/:id
func (s *Service) GetBill(ctx context.Context, id string) (*Bill, error) {
	b, err := s.findBill(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.billResponse(ctx, b)
}

// CloseBill closes a bill and returns its total and every line item charged.
// Closing a bill that is already closed returns the same result.
//
//encore:api public method=POST path=/v1/bills/:id/close
func (s *Service) CloseBill(ctx context.Context, id string) (*Bill, error) {
	b, err := s.findBill(ctx, id)
	if err != nil {
		return nil, err
	}
	if b.Status == statusOpen {
		if b, err = s.closeThroughWorkflow(ctx, b); err != nil {
			return nil, err
		}
	}
	return s.billResponse(ctx, b)
}

func (s *Service) closeThroughWorkflow(ctx context.Context, open bill) (bill, error) {
	updateErr := s.updateBill(ctx, open, client.UpdateWorkflowOptions{UpdateName: billflow.UpdateClose}, nil)

	// The update also fails when the bill closed on its own in the meantime
	// and the workflow is gone, so the stored bill decides the outcome.
	b, err := s.findBill(ctx, open.ID)
	if err != nil {
		return bill{}, err
	}
	if b.Status == statusClosed {
		return b, nil
	}
	if updateErr == nil {
		updateErr = errors.New("bill is still open after its close completed")
	}
	return bill{}, errs.WrapCode(updateErr, errs.Unavailable, "close bill")
}

func (s *Service) findBill(ctx context.Context, id string) (bill, error) {
	b, err := s.store.bill(ctx, id)
	if errors.Is(err, errBillNotFound) {
		return bill{}, &errs.Error{Code: errs.NotFound, Message: "bill not found"}
	}
	return b, err
}

// billResponse is the bill with the first page of its line items.
func (s *Service) billResponse(ctx context.Context, b bill) (*Bill, error) {
	items, err := s.lineItemList(ctx, b.ID, "", defaultPageSize)
	if err != nil {
		return nil, err
	}
	resp := billSummary(b)
	resp.LineItems = items
	return &resp, nil
}

func billSummary(b bill) Bill {
	return Bill{
		ID:          b.ID,
		CustomerID:  b.CustomerID,
		Currency:    string(b.Currency),
		Status:      b.Status,
		PeriodEnd:   b.PeriodEnd,
		CreatedAt:   b.CreatedAt,
		ClosedAt:    b.ClosedAt,
		TotalAmount: b.Total.String(),
	}
}

const (
	billIDPrefix     = "bill"
	lineItemIDPrefix = "line_item"

	maxKeyLength         = 255
	maxDescriptionLength = 500
)

// validateText requires a non-empty, bounded string that Postgres and
// Temporal will both accept: valid UTF-8 without control characters. The
// bound is in bytes because Temporal limits the Update ID, which carries the
// idempotency key, in bytes.
func validateText(field, value string, maxLength int) error {
	switch {
	case value == "":
		return invalidArgument(field + " is required")
	case len(value) > maxLength:
		return invalidArgument(fmt.Sprintf("%s must be at most %d bytes", field, maxLength))
	case !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl):
		return invalidArgument(field + " must be valid UTF-8 without control characters")
	}
	return nil
}

// newID returns a prefixed, time-ordered identifier such as
// "bill_0192f0c4a8b07c3e9d3f5a1b2c3d4e5f".
func newID(prefix string) string {
	id := uuid.Must(uuid.NewV7())
	return prefix + "_" + strings.ReplaceAll(id.String(), "-", "")
}

func invalidArgument(msg string) error {
	return &errs.Error{Code: errs.InvalidArgument, Message: msg}
}

func errIdempotencyKeyReused() error {
	return invalidArgument("Idempotency-Key was already used with a different request payload")
}

func errBillClosed() error {
	return &errs.Error{Code: errs.FailedPrecondition, Message: "bill is closed"}
}
