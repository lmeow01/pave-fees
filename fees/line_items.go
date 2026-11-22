package fees

import (
	"context"
	"net/http"
	"time"

	"encore.dev/beta/errs"
	"go.temporal.io/sdk/client"

	"github.com/lmeow01/pave-fees/fees/billflow"
	"github.com/lmeow01/pave-fees/fees/money"
)

// LineItem is the API representation of a fee accrued on a bill.
type LineItem struct {
	ID          string    `json:"id"`
	BillID      string    `json:"bill_id"`
	Description string    `json:"description"`
	Amount      string    `json:"amount"`
	Currency    string    `json:"currency"`
	CreatedAt   time.Time `json:"created_at"`

	HTTPStatus int `json:"-" encore:"httpstatus"`
}

// AddLineItemParams is the request of AddLineItem.
type AddLineItemParams struct {
	IdempotencyKey string `header:"Idempotency-Key"`

	Description string `json:"description"`
	// Amount is a positive whole number of minor units, as a string.
	Amount string `json:"amount"`
	// Currency must be the bill's currency.
	Currency string `json:"currency"`
}

// AddLineItem adds a fee to an open bill. Retrying with the same
// Idempotency-Key returns the line item that was added the first time.
//
//encore:api public method=POST path=/v1/bills/:id/line-items
func (s *Service) AddLineItem(ctx context.Context, id string, p *AddLineItemParams) (*LineItem, error) {
	item, err := p.newLineItem()
	if err != nil {
		return nil, err
	}

	stored, found, err := s.store.lineItemByKey(ctx, id, item.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	if !found {
		if stored, err = s.addThroughWorkflow(ctx, id, item); err != nil {
			return nil, err
		}
	}
	if stored.Description != item.Description || stored.Amount != item.Amount || stored.Currency != item.Currency {
		return nil, errIdempotencyKeyReused()
	}

	resp := lineItemResponse(stored)
	resp.HTTPStatus = http.StatusCreated
	return &resp, nil
}

func (p *AddLineItemParams) newLineItem() (billflow.NewLineItem, error) {
	if err := validateText("Idempotency-Key header", p.IdempotencyKey, maxKeyLength); err != nil {
		return billflow.NewLineItem{}, err
	}
	if err := validateText("description", p.Description, maxDescriptionLength); err != nil {
		return billflow.NewLineItem{}, err
	}
	currency, err := money.ParseCurrency(p.Currency)
	if err != nil {
		return billflow.NewLineItem{}, invalidArgument(err.Error())
	}
	amount, err := money.ParseAmount(p.Amount)
	if err != nil {
		return billflow.NewLineItem{}, invalidArgument(err.Error())
	}
	if amount == 0 {
		return billflow.NewLineItem{}, invalidArgument("amount must be greater than zero")
	}
	return billflow.NewLineItem{
		ID:             newID(lineItemIDPrefix),
		Description:    p.Description,
		Amount:         amount,
		Currency:       currency,
		IdempotencyKey: p.IdempotencyKey,
	}, nil
}

func (s *Service) addThroughWorkflow(ctx context.Context, billID string, item billflow.NewLineItem) (billflow.LineItem, error) {
	b, err := s.findBill(ctx, billID)
	if err != nil {
		return billflow.LineItem{}, err
	}
	if b.Status == statusClosed {
		return billflow.LineItem{}, errBillClosed()
	}

	var stored billflow.LineItem
	err = s.updateBill(ctx, b, client.UpdateWorkflowOptions{
		UpdateName: billflow.UpdateAddLineItem,
		UpdateID:   item.IdempotencyKey,
		Args:       []any{item},
	}, &stored)
	switch {
	case err == nil:
		return stored, nil
	case billflow.IsCurrencyMismatch(err):
		return billflow.LineItem{}, invalidArgument("currency does not match the bill's currency " + string(b.Currency))
	case billflow.IsBillClosed(err):
		return billflow.LineItem{}, errBillClosed()
	case billflow.IsPermanentFailure(err):
		return billflow.LineItem{}, errs.WrapCode(err, errs.Internal, "add line item")
	}

	// The update also fails when the bill closed after it was read above and
	// the workflow is gone, so the stored bill decides the outcome.
	if current, findErr := s.findBill(ctx, billID); findErr == nil && current.Status == statusClosed {
		return billflow.LineItem{}, errBillClosed()
	}
	return billflow.LineItem{}, errs.WrapCode(err, errs.Unavailable, "add line item")
}

// ListLineItemsParams is the request of ListLineItems.
type ListLineItemsParams struct {
	PageSize      int    `query:"page_size"`
	NextPageToken string `query:"next_page_token"`
}

// LineItemList is one page of a bill's line items.
type LineItemList struct {
	Data []LineItem `json:"data"`
	// NextPageToken is empty on the last page.
	NextPageToken string `json:"next_page_token"`
}

// ListLineItems returns a bill's line items, oldest first.
//
//encore:api public method=GET path=/v1/bills/:id/line-items
func (s *Service) ListLineItems(ctx context.Context, id string, p *ListLineItemsParams) (*LineItemList, error) {
	if err := validatePageToken(p.NextPageToken, lineItemIDPrefix); err != nil {
		return nil, err
	}
	size, err := pageSize(p.PageSize)
	if err != nil {
		return nil, err
	}
	if _, err := s.findBill(ctx, id); err != nil {
		return nil, err
	}
	return s.lineItemList(ctx, id, p.NextPageToken, size)
}

func (s *Service) lineItemList(ctx context.Context, billID, after string, size int) (*LineItemList, error) {
	items, err := s.store.lineItems(ctx, billID, after, size+1)
	if err != nil {
		return nil, err
	}
	page, next := splitPage(items, size, func(li billflow.LineItem) string { return li.ID })

	list := &LineItemList{Data: make([]LineItem, len(page)), NextPageToken: next}
	for i, li := range page {
		list.Data[i] = lineItemResponse(li)
	}
	return list, nil
}

func lineItemResponse(li billflow.LineItem) LineItem {
	return LineItem{
		ID:          li.ID,
		BillID:      li.BillID,
		Description: li.Description,
		Amount:      li.Amount.String(),
		Currency:    string(li.Currency),
		CreatedAt:   li.CreatedAt,
	}
}
