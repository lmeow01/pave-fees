package fees

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"encore.dev/beta/errs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
)

// These tests run the real service through its endpoints: Postgres is
// provisioned by `encore test`, and a Temporal dev server must be reachable
// (see `task test`).

func openBill(t *testing.T, currency string) *Bill {
	t.Helper()
	bill, err := CreateBill(context.Background(), &CreateBillParams{
		IdempotencyKey: newID("key"),
		CustomerID:     "customer_1",
		Currency:       currency,
		PeriodEnd:      time.Now().Add(30 * 24 * time.Hour),
	})
	require.NoError(t, err)
	return bill
}

func fee(amount, currency string) *AddLineItemParams {
	return &AddLineItemParams{
		IdempotencyKey: newID("key"),
		Description:    "card processing fee",
		Amount:         amount,
		Currency:       currency,
	}
}

func requireCode(t *testing.T, want errs.ErrCode, err error) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, want, errs.Code(err), "error: %v", err)
}

func TestBillLifecycle(t *testing.T) {
	ctx := context.Background()

	bill := openBill(t, "USD")
	assert.Equal(t, "open", bill.Status)
	assert.Equal(t, "0", bill.TotalAmount)
	assert.Empty(t, bill.LineItems.Data)
	assert.Nil(t, bill.ClosedAt)

	first, err := AddLineItem(ctx, bill.ID, fee("1050", "USD"))
	require.NoError(t, err)
	assert.Equal(t, bill.ID, first.BillID)
	assert.Equal(t, "1050", first.Amount)
	second, err := AddLineItem(ctx, bill.ID, fee("295", "USD"))
	require.NoError(t, err)

	accruing, err := GetBill(ctx, bill.ID)
	require.NoError(t, err)
	assert.Equal(t, "open", accruing.Status)
	assert.Equal(t, "1345", accruing.TotalAmount)

	closed, err := CloseBill(ctx, bill.ID)
	require.NoError(t, err)
	assert.Equal(t, "closed", closed.Status)
	assert.Equal(t, "1345", closed.TotalAmount)
	assert.NotNil(t, closed.ClosedAt)
	require.Len(t, closed.LineItems.Data, 2)
	assert.Equal(t, []string{first.ID, second.ID}, []string{closed.LineItems.Data[0].ID, closed.LineItems.Data[1].ID})

	_, err = AddLineItem(ctx, bill.ID, fee("1", "USD"))
	requireCode(t, errs.FailedPrecondition, err)

	closedAgain, err := CloseBill(ctx, bill.ID)
	require.NoError(t, err)
	assert.Equal(t, closed, closedAgain)
}

func TestBillsKeepTheirOwnCurrency(t *testing.T) {
	ctx := context.Background()
	gel := openBill(t, "GEL")

	_, err := AddLineItem(ctx, gel.ID, fee("500", "USD"))
	requireCode(t, errs.InvalidArgument, err)

	_, err = AddLineItem(ctx, gel.ID, fee("500", "GEL"))
	require.NoError(t, err)

	closed, err := CloseBill(ctx, gel.ID)
	require.NoError(t, err)
	assert.Equal(t, "GEL", closed.Currency)
	assert.Equal(t, "500", closed.TotalAmount)
	require.Len(t, closed.LineItems.Data, 1)
	assert.Equal(t, "GEL", closed.LineItems.Data[0].Currency)
}

func TestCreateBillIsIdempotent(t *testing.T) {
	ctx := context.Background()
	params := &CreateBillParams{
		IdempotencyKey: newID("key"),
		CustomerID:     "customer_1",
		Currency:       "USD",
		PeriodEnd:      time.Now().Add(time.Hour),
	}

	first, err := CreateBill(ctx, params)
	require.NoError(t, err)
	retried, err := CreateBill(ctx, params)
	require.NoError(t, err)
	assert.Equal(t, first.ID, retried.ID)

	changed := *params
	changed.Currency = "GEL"
	_, err = CreateBill(ctx, &changed)
	requireCode(t, errs.InvalidArgument, err)
}

func TestCreateBillRetryAfterTheBillClosedReturnsTheBill(t *testing.T) {
	ctx := context.Background()
	params := &CreateBillParams{
		IdempotencyKey: newID("key"),
		CustomerID:     "customer_1",
		Currency:       "USD",
		PeriodEnd:      time.Now().Add(time.Second),
	}
	first, err := CreateBill(ctx, params)
	require.NoError(t, err)

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		got, err := GetBill(ctx, first.ID)
		require.NoError(c, err)
		require.Equal(c, "closed", got.Status)
	}, 30*time.Second, 200*time.Millisecond)

	retried, err := CreateBill(ctx, params)
	require.NoError(t, err, "period_end is in the past by now, but this is a replay")
	assert.Equal(t, first.ID, retried.ID)
	assert.Equal(t, "closed", retried.Status)
}

func TestCreateBillValidation(t *testing.T) {
	valid := func() *CreateBillParams {
		return &CreateBillParams{
			IdempotencyKey: newID("key"),
			CustomerID:     "customer_1",
			Currency:       "USD",
			PeriodEnd:      time.Now().Add(time.Hour),
		}
	}
	tests := map[string]func(p *CreateBillParams){
		"missing idempotency key": func(p *CreateBillParams) { p.IdempotencyKey = "" },
		"missing customer":        func(p *CreateBillParams) { p.CustomerID = "" },
		"unsupported currency":    func(p *CreateBillParams) { p.Currency = "EUR" },
		"lower-case currency":     func(p *CreateBillParams) { p.Currency = "usd" },
		"period already ended":    func(p *CreateBillParams) { p.PeriodEnd = time.Now().Add(-time.Minute) },
	}
	for name, breakIt := range tests {
		t.Run(name, func(t *testing.T) {
			p := valid()
			breakIt(p)
			_, err := CreateBill(context.Background(), p)
			requireCode(t, errs.InvalidArgument, err)
		})
	}
}

func TestAddLineItemIsIdempotent(t *testing.T) {
	ctx := context.Background()
	bill := openBill(t, "USD")
	params := fee("1050", "USD")

	first, err := AddLineItem(ctx, bill.ID, params)
	require.NoError(t, err)
	retried, err := AddLineItem(ctx, bill.ID, params)
	require.NoError(t, err)
	assert.Equal(t, first, retried)

	changed := *params
	changed.Amount = "9999"
	_, err = AddLineItem(ctx, bill.ID, &changed)
	requireCode(t, errs.InvalidArgument, err)

	closed, err := CloseBill(ctx, bill.ID)
	require.NoError(t, err)
	assert.Equal(t, "1050", closed.TotalAmount)
	assert.Len(t, closed.LineItems.Data, 1)

	afterClose, err := AddLineItem(ctx, bill.ID, params)
	require.NoError(t, err, "a retry of an accepted line item still succeeds after the bill closed")
	assert.Equal(t, first, afterClose)
}

func TestAddLineItemValidation(t *testing.T) {
	bill := openBill(t, "USD")
	tests := map[string]func(p *AddLineItemParams){
		"missing idempotency key": func(p *AddLineItemParams) { p.IdempotencyKey = "" },
		"missing description":     func(p *AddLineItemParams) { p.Description = "" },
		"NUL in description":      func(p *AddLineItemParams) { p.Description = "fee\x00" },
		"oversized description":   func(p *AddLineItemParams) { p.Description = strings.Repeat("a", 501) },
		"oversized key":           func(p *AddLineItemParams) { p.IdempotencyKey = strings.Repeat("k", 256) },
		"decimal amount":          func(p *AddLineItemParams) { p.Amount = "10.50" },
		"zero amount":             func(p *AddLineItemParams) { p.Amount = "0" },
		"negative amount":         func(p *AddLineItemParams) { p.Amount = "-5" },
		"unsupported currency":    func(p *AddLineItemParams) { p.Currency = "EUR" },
		"other bill currency":     func(p *AddLineItemParams) { p.Currency = "GEL" },
	}
	for name, breakIt := range tests {
		t.Run(name, func(t *testing.T) {
			p := fee("100", "USD")
			breakIt(p)
			_, err := AddLineItem(context.Background(), bill.ID, p)
			requireCode(t, errs.InvalidArgument, err)
		})
	}

	unchanged, err := GetBill(context.Background(), bill.ID)
	require.NoError(t, err)
	assert.Empty(t, unchanged.LineItems.Data)
}

func TestUnknownBill(t *testing.T) {
	ctx := context.Background()
	id := newID("bill")

	_, err := GetBill(ctx, id)
	requireCode(t, errs.NotFound, err)
	_, err = AddLineItem(ctx, id, fee("100", "USD"))
	requireCode(t, errs.NotFound, err)
	_, err = CloseBill(ctx, id)
	requireCode(t, errs.NotFound, err)
}

func TestBillClosesItselfAtPeriodEnd(t *testing.T) {
	ctx := context.Background()
	periodEnd := time.Now().Add(2 * time.Second)
	bill, err := CreateBill(ctx, &CreateBillParams{
		IdempotencyKey: newID("key"),
		CustomerID:     "customer_1",
		Currency:       "USD",
		PeriodEnd:      periodEnd,
	})
	require.NoError(t, err)
	_, err = AddLineItem(ctx, bill.ID, fee("700", "USD"))
	require.NoError(t, err)

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		got, err := GetBill(ctx, bill.ID)
		require.NoError(c, err)
		require.Equal(c, "closed", got.Status)
		assert.Equal(c, "700", got.TotalAmount)
	}, 30*time.Second, 200*time.Millisecond)

	_, err = AddLineItem(ctx, bill.ID, fee("1", "USD"))
	requireCode(t, errs.FailedPrecondition, err)
}

// Many writers keep adding line items while the bill is being closed.
// Whatever the interleaving, the closed bill must charge exactly the line
// items whose callers were told they were accepted.
func TestConcurrentAddsRacingClose(t *testing.T) {
	ctx := context.Background()
	bill := openBill(t, "USD")

	const writers = 16
	var (
		wg            sync.WaitGroup
		mu            sync.Mutex
		accepted      = map[string]int64{}
		refused       int
		firstAccepted = make(chan struct{})
		announce      sync.Once
	)
	for w := range writers {
		wg.Go(func() {
			// Each writer adds until the bill refuses it, so every writer is
			// mid-request when the close lands.
			for n := 1; ; n++ {
				amount := int64(w*1000 + n)
				item, err := AddLineItem(ctx, bill.ID, fee(strconv.FormatInt(amount, 10), "USD"))
				if err != nil {
					assert.Equal(t, errs.FailedPrecondition, errs.Code(err), "unexpected error: %v", err)
					mu.Lock()
					refused++
					mu.Unlock()
					return
				}
				mu.Lock()
				accepted[item.ID] = amount
				mu.Unlock()
				announce.Do(func() { close(firstAccepted) })
			}
		})
	}
	writersDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(writersDone)
	}()
	select {
	case <-firstAccepted:
	case <-writersDone:
		t.Fatal("every writer was refused before any line item was accepted")
	}
	_, err := CloseBill(ctx, bill.ID)
	require.NoError(t, err)
	<-writersDone

	require.NotEmpty(t, accepted, "no line item was accepted before the close")
	require.Equal(t, writers, refused, "every writer ends by being refused")

	closed, err := GetBill(ctx, bill.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", closed.Status)

	var wantTotal int64
	for _, amount := range accepted {
		wantTotal += amount
	}
	assert.Equal(t, strconv.FormatInt(wantTotal, 10), closed.TotalAmount)

	charged := map[string]bool{}
	token := ""
	for {
		page, err := ListLineItems(ctx, bill.ID, &ListLineItemsParams{PageSize: maxPageSize, NextPageToken: token})
		require.NoError(t, err)
		for _, li := range page.Data {
			charged[li.ID] = true
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	assert.Len(t, charged, len(accepted))
	for id := range accepted {
		assert.True(t, charged[id], "accepted line item %s is missing from the closed bill", id)
	}
}

// billRunID is the run currently executing the bill's workflow.
func billRunID(t *testing.T, billID string) string {
	t.Helper()
	c, err := client.Dial(client.Options{HostPort: cfg.TemporalHostPort(), Namespace: cfg.TemporalNamespace()})
	require.NoError(t, err)
	defer c.Close()
	desc, err := c.DescribeWorkflow(context.Background(), billID, "")
	require.NoError(t, err)
	return desc.WorkflowExecution.RunID
}

// The Temporal server that scripts/with-temporal.sh starts suggests
// continue-as-new after a few dozen history events, so this bill's workflow
// rolls over to a new run several times while it is being written to.
func TestBillKeepsAccruingAcrossWorkflowRollovers(t *testing.T) {
	ctx := context.Background()
	bill := openBill(t, "USD")
	firstRun := billRunID(t, bill.ID)

	const writers, itemsPerWriter = 4, 25
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			for range itemsPerWriter {
				_, err := AddLineItem(ctx, bill.ID, fee("10", "USD"))
				assert.NoError(t, err)
			}
		})
	}
	wg.Wait()

	// CI always runs against the server scripts/with-temporal.sh starts. A
	// Temporal server with default settings would not roll over this soon.
	if os.Getenv("CI") != "" {
		require.NotEqual(t, firstRun, billRunID(t, bill.ID), "the workflow did not continue as new")
	}

	closed, err := CloseBill(ctx, bill.ID)
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(writers*itemsPerWriter*10), closed.TotalAmount)
	assert.Len(t, closed.LineItems.Data, writers*itemsPerWriter)
}

func TestLineItemsArePaged(t *testing.T) {
	ctx := context.Background()
	bill := openBill(t, "USD")
	var added []string
	for range 5 {
		li, err := AddLineItem(ctx, bill.ID, fee("100", "USD"))
		require.NoError(t, err)
		added = append(added, li.ID)
	}

	var listed []string
	token := ""
	for pages := 0; ; pages++ {
		require.Less(t, pages, 3, "more pages than 5 items at 2 per page can fill")
		page, err := ListLineItems(ctx, bill.ID, &ListLineItemsParams{PageSize: 2, NextPageToken: token})
		require.NoError(t, err)
		require.LessOrEqual(t, len(page.Data), 2)
		for _, li := range page.Data {
			listed = append(listed, li.ID)
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	assert.Equal(t, added, listed)

	whole, err := GetBill(ctx, bill.ID)
	require.NoError(t, err)
	assert.Equal(t, "500", whole.TotalAmount, "the total covers every line item, not one page")
	assert.Len(t, whole.LineItems.Data, 5)
	assert.Empty(t, whole.LineItems.NextPageToken)
}

func TestListBills(t *testing.T) {
	ctx := context.Background()
	customer := newID("customer")
	var created []string
	for range 3 {
		bill, err := CreateBill(ctx, &CreateBillParams{
			IdempotencyKey: newID("key"),
			CustomerID:     customer,
			Currency:       "USD",
			PeriodEnd:      time.Now().Add(time.Hour),
		})
		require.NoError(t, err)
		created = append(created, bill.ID)
	}
	openBill(t, "USD") // another customer's bill
	_, err := AddLineItem(ctx, created[0], fee("250", "USD"))
	require.NoError(t, err)
	_, err = CloseBill(ctx, created[0])
	require.NoError(t, err)

	first, err := ListBills(ctx, &ListBillsParams{CustomerID: customer, PageSize: 2})
	require.NoError(t, err)
	require.Len(t, first.Data, 2)
	require.NotEmpty(t, first.NextPageToken)
	second, err := ListBills(ctx, &ListBillsParams{CustomerID: customer, PageSize: 2, NextPageToken: first.NextPageToken})
	require.NoError(t, err)
	require.Len(t, second.Data, 1)
	assert.Empty(t, second.NextPageToken)

	newestFirst := []string{first.Data[0].ID, first.Data[1].ID, second.Data[0].ID}
	assert.Equal(t, []string{created[2], created[1], created[0]}, newestFirst)
	assert.Nil(t, first.Data[0].LineItems, "listed bills carry no line items")

	closed, err := ListBills(ctx, &ListBillsParams{CustomerID: customer, Status: "closed"})
	require.NoError(t, err)
	require.Len(t, closed.Data, 1)
	assert.Equal(t, created[0], closed.Data[0].ID)
	assert.Equal(t, "250", closed.Data[0].TotalAmount)
}

func TestListValidation(t *testing.T) {
	ctx := context.Background()
	bill := openBill(t, "USD")

	listBills := map[string]*ListBillsParams{
		"missing customer":   {},
		"unknown status":     {CustomerID: "customer_1", Status: "void"},
		"negative page size": {CustomerID: "customer_1", PageSize: -1},
		"oversized page":     {CustomerID: "customer_1", PageSize: maxPageSize + 1},
		"foreign page token": {CustomerID: "customer_1", NextPageToken: "line_item_1"},
	}
	for name, params := range listBills {
		t.Run("bills/"+name, func(t *testing.T) {
			_, err := ListBills(ctx, params)
			requireCode(t, errs.InvalidArgument, err)
		})
	}

	_, err := ListLineItems(ctx, bill.ID, &ListLineItemsParams{PageSize: maxPageSize + 1})
	requireCode(t, errs.InvalidArgument, err)
	_, err = ListLineItems(ctx, bill.ID, &ListLineItemsParams{NextPageToken: "bill_1"})
	requireCode(t, errs.InvalidArgument, err)
	_, err = ListLineItems(ctx, newID("bill"), &ListLineItemsParams{})
	requireCode(t, errs.NotFound, err)
}

// A bill row without a workflow is what a crash between CreateBill's two
// steps leaves behind when the client never retries.
func billWithoutWorkflow(t *testing.T, periodEnd time.Time) string {
	t.Helper()
	b, err := store{db: db}.createBill(context.Background(), newBill{
		ID:             newID("bill"),
		CustomerID:     "customer_1",
		Currency:       "USD",
		PeriodEnd:      periodEnd,
		IdempotencyKey: newID("key"),
	})
	require.NoError(t, err)
	return b.ID
}

func TestBillWithoutWorkflowIsRepairedByTheNextWrite(t *testing.T) {
	ctx := context.Background()

	t.Run("add then close", func(t *testing.T) {
		id := billWithoutWorkflow(t, time.Now().Add(time.Hour))
		_, err := AddLineItem(ctx, id, fee("700", "USD"))
		require.NoError(t, err)
		closed, err := CloseBill(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, "closed", closed.Status)
		assert.Equal(t, "700", closed.TotalAmount)
	})

	t.Run("close", func(t *testing.T) {
		id := billWithoutWorkflow(t, time.Now().Add(time.Hour))
		closed, err := CloseBill(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, "closed", closed.Status)
		assert.Equal(t, "0", closed.TotalAmount)
	})

	t.Run("add after the period ended", func(t *testing.T) {
		id := billWithoutWorkflow(t, time.Now().Add(-time.Hour))
		_, err := AddLineItem(ctx, id, fee("700", "USD"))
		requireCode(t, errs.FailedPrecondition, err)
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			bill, err := GetBill(ctx, id)
			require.NoError(c, err)
			assert.Equal(c, "closed", bill.Status)
			assert.Equal(c, "0", bill.TotalAmount)
		}, 10*time.Second, 50*time.Millisecond)
	})
}

func TestServerSideFailureDoesNotReachTheCaller(t *testing.T) {
	bill := openBill(t, "USD")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := GetBill(ctx, bill.ID)

	require.Error(t, err)
	assert.NotContains(t, []errs.ErrCode{errs.InvalidArgument, errs.NotFound, errs.FailedPrecondition}, errs.Code(err))
	assert.NotContains(t, err.Error(), "query bill")
	assert.NotContains(t, err.Error(), "context canceled")
}

// stuckBill is a bill whose workflow makes no progress: its run waits on a
// task queue that no worker polls. Writes to it wait at most a second. The
// returned function ends the stuck run and restores the wait limit; it also
// runs when the test ends.
func stuckBill(t *testing.T, periodEnd time.Time) (string, func()) {
	t.Helper()
	ctx := context.Background()
	limit := updateWaitLimit
	updateWaitLimit = time.Second

	id := billWithoutWorkflow(t, periodEnd)
	c, err := client.Dial(client.Options{HostPort: cfg.TemporalHostPort(), Namespace: cfg.TemporalNamespace()})
	require.NoError(t, err)
	stuck := billWorkflowOptions(id)
	stuck.TaskQueue = "unpolled"
	_, err = c.ExecuteWorkflow(ctx, stuck, "Bill")
	require.NoError(t, err)

	var once sync.Once
	unstick := func() {
		once.Do(func() {
			updateWaitLimit = limit
			assert.NoError(t, c.TerminateWorkflow(ctx, id, "", "test"))
			c.Close()
		})
	}
	t.Cleanup(unstick)
	return id, unstick
}

func TestOverdueBillsAreClosedBySweep(t *testing.T) {
	ctx := context.Background()
	overdue := billWithoutWorkflow(t, time.Now().Add(-time.Hour))
	_, err := store{db: db}.InsertLineItem(ctx, usdLineItem(overdue))
	require.NoError(t, err)
	notYetDue := billWithoutWorkflow(t, time.Now().Add(time.Hour))
	stuck, _ := stuckBill(t, time.Now().Add(-2*time.Hour))

	swept, err := CloseOverdueBills(ctx)
	require.NoError(t, err)

	assert.Contains(t, swept.Failed, stuck, "a bill that cannot close is reported")
	assert.Contains(t, swept.Closed, overdue, "and does not hold up the bills behind it")
	assert.NotContains(t, swept.Closed, notYetDue)
	closed, err := GetBill(ctx, overdue)
	require.NoError(t, err)
	assert.Equal(t, "closed", closed.Status)
	assert.Equal(t, "100", closed.TotalAmount)
	stillOpen, err := GetBill(ctx, notYetDue)
	require.NoError(t, err)
	assert.Equal(t, "open", stillOpen.Status)
}

func TestWriteToStuckBillIsUnavailable(t *testing.T) {
	ctx := context.Background()
	id, unstick := stuckBill(t, time.Now().Add(time.Hour))

	add := fee("700", "USD")
	_, err := AddLineItem(ctx, id, add)
	requireCode(t, errs.Unavailable, err)
	_, err = CloseBill(ctx, id)
	requireCode(t, errs.Unavailable, err)

	unstick()
	_, err = AddLineItem(ctx, id, add)
	require.NoError(t, err, "a retry with the same key succeeds once the bill's workflow runs again")
	closed, err := CloseBill(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "700", closed.TotalAmount)
}
