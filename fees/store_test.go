package fees

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lmeow01/pave-fees/fees/billflow"
	"github.com/lmeow01/pave-fees/fees/money"
)

func TestStoreReportsRefusedDataAsPermanent(t *testing.T) {
	ctx := context.Background()
	bill := openBill(t, "USD")

	_, err := store{db: db}.InsertLineItem(ctx, billflow.LineItem{
		ID:             newID("line_item"),
		BillID:         bill.ID,
		Description:    "nul \x00 byte",
		Amount:         money.Amount(100),
		Currency:       money.USD,
		IdempotencyKey: newID("key"),
		CreatedAt:      time.Now(),
	})

	require.ErrorIs(t, err, billflow.ErrPermanent)
}

func TestStoreReportsClosingAnUnknownBillAsPermanent(t *testing.T) {
	err := store{db: db}.CloseBill(context.Background(), newID("bill"), time.Now())

	require.ErrorIs(t, err, billflow.ErrPermanent)
	require.ErrorIs(t, err, errBillNotFound)
}

func usdLineItem(billID string) billflow.LineItem {
	return billflow.LineItem{
		ID:             newID(lineItemIDPrefix),
		BillID:         billID,
		Description:    "card processing fee",
		Amount:         money.Amount(100),
		Currency:       money.USD,
		IdempotencyKey: newID("key"),
		CreatedAt:      time.Now(),
	}
}

func TestStoreKeepsTheFirstLineItemOfAnIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	bill := openBill(t, "USD")
	st := store{db: db}
	first := usdLineItem(bill.ID)
	retry := first
	retry.ID = newID(lineItemIDPrefix)

	stored, err := st.InsertLineItem(ctx, first)
	require.NoError(t, err)
	storedAgain, err := st.InsertLineItem(ctx, retry)
	require.NoError(t, err)

	require.Equal(t, first.ID, stored.ID)
	require.Equal(t, first.ID, storedAgain.ID)
	items, err := st.lineItems(ctx, bill.ID, "", 10)
	require.NoError(t, err)
	require.Len(t, items, 1)
}

func TestStoreRefusesALineItemOnAClosedBill(t *testing.T) {
	ctx := context.Background()
	bill := openBill(t, "USD")
	_, err := CloseBill(ctx, bill.ID)
	require.NoError(t, err)
	st := store{db: db}

	_, err = st.InsertLineItem(ctx, usdLineItem(bill.ID))

	require.ErrorIs(t, err, billflow.ErrBillNotOpen)
	items, err := st.lineItems(ctx, bill.ID, "", 10)
	require.NoError(t, err)
	require.Empty(t, items)
}
