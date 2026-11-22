package billflow_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/lmeow01/pave-fees/fees/billflow"
	"github.com/lmeow01/pave-fees/fees/money"
)

var (
	periodStart = time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	periodEnd   = time.Date(2026, time.December, 1, 0, 0, 0, 0, time.UTC)
)

// fakeStore keeps what the workflow's activities persist, so tests can assert
// on the outcome the way a reader of the database would see it.
type fakeStore struct {
	mu       sync.Mutex
	items    []billflow.LineItem
	closedAt time.Time
	// itemsAtClose is how many line items existed when the bill was closed.
	itemsAtClose int
	insertDelay  time.Duration
	insertErr    error
}

func (s *fakeStore) InsertLineItem(_ context.Context, item billflow.LineItem) (billflow.LineItem, error) {
	time.Sleep(s.insertDelay)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.insertErr != nil {
		return billflow.LineItem{}, s.insertErr
	}
	if !s.closedAt.IsZero() {
		return billflow.LineItem{}, billflow.ErrBillNotOpen
	}
	for _, existing := range s.items {
		if existing.IdempotencyKey == item.IdempotencyKey {
			return existing, nil
		}
	}
	s.items = append(s.items, item)
	return item, nil
}

func (s *fakeStore) CloseBill(_ context.Context, _ string, closedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closedAt.IsZero() {
		s.closedAt = closedAt
		s.itemsAtClose = len(s.items)
	}
	return nil
}

// updateOutcome records how the workflow answered one update.
type updateOutcome struct {
	rejection error
	result    any
	err       error
	completed bool
}

func (o *updateOutcome) callbacks() *testsuite.TestUpdateCallback {
	return &testsuite.TestUpdateCallback{
		OnAccept: func() {},
		OnReject: func(err error) { o.rejection = err },
		OnComplete: func(result any, err error) {
			o.result, o.err, o.completed = result, err, true
		},
	}
}

type billTest struct {
	env   *testsuite.TestWorkflowEnvironment
	store *fakeStore
}

func newBillTest(t *testing.T) *billTest {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetStartTime(periodStart)
	store := &fakeStore{}
	env.RegisterActivity(&billflow.Activities{Store: store})
	return &billTest{env: env, store: store}
}

// add sends an add-line-item update one hour into the period.
func (bt *billTest) add(item billflow.NewLineItem) *updateOutcome {
	outcome := &updateOutcome{}
	bt.env.RegisterDelayedCallback(func() {
		bt.env.UpdateWorkflow(billflow.UpdateAddLineItem, item.IdempotencyKey, outcome.callbacks(), item)
	}, time.Hour)
	return outcome
}

func (bt *billTest) closeAfter(delay time.Duration) *updateOutcome {
	outcome := &updateOutcome{}
	bt.env.RegisterDelayedCallback(func() {
		bt.env.UpdateWorkflow(billflow.UpdateClose, "", outcome.callbacks())
	}, delay)
	return outcome
}

func (bt *billTest) run(t *testing.T) {
	t.Helper()
	bt.env.ExecuteWorkflow(billflow.Bill, billflow.Params{
		BillID:    "bill_1",
		Currency:  money.USD,
		PeriodEnd: periodEnd,
	})
	require.True(t, bt.env.IsWorkflowCompleted())
	require.NoError(t, bt.env.GetWorkflowError())
}

func usdItem(key string) billflow.NewLineItem {
	return billflow.NewLineItem{
		ID:             "line_item_" + key,
		Description:    "card processing fee",
		Amount:         250,
		Currency:       money.USD,
		IdempotencyKey: key,
	}
}

func TestAddLineItemToOpenBill(t *testing.T) {
	bt := newBillTest(t)
	added := bt.add(usdItem("k1"))
	bt.run(t)

	require.True(t, added.completed)
	require.NoError(t, added.err)
	want := billflow.LineItem{
		ID:             "line_item_k1",
		BillID:         "bill_1",
		Description:    "card processing fee",
		Amount:         250,
		Currency:       money.USD,
		IdempotencyKey: "k1",
		CreatedAt:      periodStart.Add(time.Hour),
	}
	assert.Equal(t, want, added.result)
	assert.Equal(t, []billflow.LineItem{want}, bt.store.items)
}

func TestAddLineItemInAnotherCurrencyIsRejected(t *testing.T) {
	bt := newBillTest(t)
	item := usdItem("k1")
	item.Currency = money.GEL
	added := bt.add(item)
	bt.run(t)

	require.Error(t, added.rejection)
	assert.True(t, billflow.IsCurrencyMismatch(added.rejection))
	assert.Empty(t, bt.store.items)
}

func TestCloseBeforePeriodEnd(t *testing.T) {
	bt := newBillTest(t)
	bt.add(usdItem("k1"))
	closed := bt.closeAfter(2 * time.Hour)
	bt.run(t)

	require.True(t, closed.completed)
	require.NoError(t, closed.err)
	assert.Equal(t, periodStart.Add(2*time.Hour), bt.store.closedAt)
	assert.Equal(t, 1, bt.store.itemsAtClose)
}

func TestBillClosesItselfAtPeriodEnd(t *testing.T) {
	bt := newBillTest(t)
	bt.add(usdItem("k1"))
	bt.run(t)

	assert.Equal(t, periodEnd, bt.store.closedAt)
	assert.Equal(t, 1, bt.store.itemsAtClose)
}

func TestAddLineItemAfterCloseRequestedIsRejected(t *testing.T) {
	bt := newBillTest(t)
	bt.closeAfter(time.Hour)
	late := bt.add(usdItem("late"))
	bt.run(t)

	require.Error(t, late.rejection)
	assert.True(t, billflow.IsBillClosed(late.rejection))
	assert.Empty(t, bt.store.items)
}

func TestCloseWaitsForLineItemsStillBeingWritten(t *testing.T) {
	bt := newBillTest(t)
	bt.store.insertDelay = 50 * time.Millisecond
	added := bt.add(usdItem("k1"))
	bt.closeAfter(time.Hour)
	bt.run(t)

	require.NoError(t, added.err)
	assert.Len(t, bt.store.items, 1)
	assert.Equal(t, 1, bt.store.itemsAtClose)
}

// A retry that reaches the workflow as a new update, as it does after the
// workflow continued as new, must not store the line item twice.
func TestRetriedAddLineItemIsStoredOnce(t *testing.T) {
	bt := newBillTest(t)
	first := bt.add(usdItem("k1"))
	retry := &updateOutcome{}
	bt.env.RegisterDelayedCallback(func() {
		bt.env.UpdateWorkflow(billflow.UpdateAddLineItem, "another-update-id", retry.callbacks(), usdItem("k1"))
	}, 2*time.Hour)
	bt.run(t)

	require.NoError(t, first.err)
	require.NoError(t, retry.err)
	assert.Equal(t, first.result, retry.result)
	assert.Len(t, bt.store.items, 1)
}

func TestPermanentStoreFailureDoesNotHoldTheBillOpen(t *testing.T) {
	bt := newBillTest(t)
	bt.store.insertErr = fmt.Errorf("%w: invalid byte sequence", billflow.ErrPermanent)
	added := bt.add(usdItem("k1"))
	bt.closeAfter(2 * time.Hour)
	bt.run(t)

	require.Error(t, added.err)
	assert.Equal(t, periodStart.Add(2*time.Hour), bt.store.closedAt)
	assert.Empty(t, bt.store.items)
}

func TestStoreRefusalSurfacesAsBillClosed(t *testing.T) {
	activities := &billflow.Activities{Store: &fakeStore{closedAt: periodEnd}}
	_, err := activities.InsertLineItem(context.Background(), billflow.LineItem{})

	require.Error(t, err)
	assert.True(t, billflow.IsBillClosed(err))
	assert.True(t, errors.Is(err, billflow.ErrBillNotOpen))
}
