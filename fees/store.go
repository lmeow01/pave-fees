package fees

import (
	"context"
	"errors"
	"strings"
	"time"

	"encore.dev/storage/sqldb"
	"github.com/reconquest/karma-go"

	"github.com/lmeow01/pave-fees/fees/billflow"
	"github.com/lmeow01/pave-fees/fees/money"
)

var db = sqldb.NewDatabase("fees", sqldb.DatabaseConfig{
	Migrations: "./migrations",
})

var errBillNotFound = errors.New("bill not found")

const (
	statusOpen   = "open"
	statusClosed = "closed"
)

type bill struct {
	ID         string
	CustomerID string
	Currency   money.Currency
	Status     string
	PeriodEnd  time.Time
	CreatedAt  time.Time
	// ClosedAt is set once the bill is closed.
	ClosedAt *time.Time
	// Total is final once the bill is closed. On an open bill it is the sum
	// accrued so far.
	Total money.Amount
}

type newBill struct {
	ID             string
	CustomerID     string
	Currency       money.Currency
	PeriodEnd      time.Time
	IdempotencyKey string
}

// store is the Postgres persistence of bills and line items. It also
// implements billflow.Store for the workflow's activities.
type store struct {
	db *sqldb.Database
}

var _ billflow.Store = store{}

const selectBill = `
	SELECT id, customer_id, currency, status, period_end, created_at, closed_at,
	       COALESCE(total_minor, (
	           SELECT COALESCE(SUM(amount_minor), 0) FROM line_items WHERE bill_id = bills.id
	       ))::BIGINT
	FROM bills`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanBill(row rowScanner) (bill, error) {
	var b bill
	err := row.Scan(&b.ID, &b.CustomerID, &b.Currency, &b.Status, &b.PeriodEnd, &b.CreatedAt, &b.ClosedAt, &b.Total)
	if errors.Is(err, sqldb.ErrNoRows) {
		return bill{}, errBillNotFound
	}
	return b, err
}

// billByKey finds the bill a previous request with the same idempotency key
// created, if any.
func (s store) billByKey(ctx context.Context, idempotencyKey string) (bill, bool, error) {
	b, err := scanBill(s.db.QueryRow(ctx, selectBill+` WHERE idempotency_key = $1`, idempotencyKey))
	if errors.Is(err, errBillNotFound) {
		return bill{}, false, nil
	}
	if err != nil {
		return bill{}, false, karma.Format(err, "query bill by idempotency key")
	}
	return b, true, nil
}

// createBill stores a new open bill. If a concurrent request created a bill
// with the same idempotency key first, that bill is returned and nothing is
// written.
func (s store) createBill(ctx context.Context, nb newBill) (bill, error) {
	_, err := s.db.Exec(ctx, `
		INSERT INTO bills (id, customer_id, currency, status, period_end, idempotency_key)
		VALUES ($1, $2, $3, 'open', $4, $5)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		nb.ID, nb.CustomerID, nb.Currency, nb.PeriodEnd, nb.IdempotencyKey)
	if err != nil {
		return bill{}, karma.Format(err, "insert bill")
	}
	b, err := scanBill(s.db.QueryRow(ctx, selectBill+` WHERE idempotency_key = $1`, nb.IdempotencyKey))
	if err != nil {
		return bill{}, karma.Format(err, "read back bill")
	}
	return b, nil
}

func (s store) bill(ctx context.Context, id string) (bill, error) {
	b, err := scanBill(s.db.QueryRow(ctx, selectBill+` WHERE id = $1`, id))
	if err != nil && !errors.Is(err, errBillNotFound) {
		return bill{}, karma.Format(err, "query bill")
	}
	return b, err
}

// billsFilter selects a customer's bills. An empty Status matches every
// status, and an empty Before starts from the newest bill.
type billsFilter struct {
	CustomerID string
	Status     string
	Before     string
}

// bills returns up to limit bills, newest first. Bill IDs are time-ordered,
// so the ID is both the sort key and the position to continue from.
func (s store) bills(ctx context.Context, f billsFilter, limit int) ([]bill, error) {
	args := []any{f.CustomerID, f.Status, limit}
	beforeCursor := ""
	if f.Before != "" {
		beforeCursor = ` AND id < $4`
		args = append(args, f.Before)
	}
	rows, err := s.db.Query(ctx, selectBill+`
		WHERE customer_id = $1
		  AND ($2 = '' OR status = $2)`+beforeCursor+`
		ORDER BY id DESC
		LIMIT $3`,
		args...)
	if err != nil {
		return nil, karma.Format(err, "query bills")
	}
	defer rows.Close()

	bills := []bill{}
	for rows.Next() {
		b, err := scanBill(rows)
		if err != nil {
			return nil, karma.Format(err, "scan bill")
		}
		bills = append(bills, b)
	}
	if err := rows.Err(); err != nil {
		return nil, karma.Format(err, "read bills")
	}
	return bills, nil
}

const selectLineItem = `
	SELECT id, bill_id, description, amount_minor, currency, idempotency_key, created_at
	FROM line_items`

func scanLineItem(row rowScanner) (billflow.LineItem, error) {
	var li billflow.LineItem
	err := row.Scan(&li.ID, &li.BillID, &li.Description, &li.Amount, &li.Currency, &li.IdempotencyKey, &li.CreatedAt)
	return li, err
}

// lineItems returns up to limit line items of a bill, oldest first, starting
// after the line item with ID after. Line item IDs are time-ordered, so the
// ID is both the sort key and the position to continue from.
func (s store) lineItems(ctx context.Context, billID, after string, limit int) ([]billflow.LineItem, error) {
	rows, err := s.db.Query(ctx, selectLineItem+`
		WHERE bill_id = $1 AND id > $2
		ORDER BY id
		LIMIT $3`,
		billID, after, limit)
	if err != nil {
		return nil, karma.Format(err, "query line items")
	}
	defer rows.Close()

	items := []billflow.LineItem{}
	for rows.Next() {
		li, err := scanLineItem(rows)
		if err != nil {
			return nil, karma.Format(err, "scan line item")
		}
		items = append(items, li)
	}
	if err := rows.Err(); err != nil {
		return nil, karma.Format(err, "read line items")
	}
	return items, nil
}

// lineItemByKey finds the line item a previous request with the same
// idempotency key added to the bill, if any.
func (s store) lineItemByKey(ctx context.Context, billID, idempotencyKey string) (billflow.LineItem, bool, error) {
	li, err := scanLineItem(s.db.QueryRow(ctx,
		selectLineItem+` WHERE bill_id = $1 AND idempotency_key = $2`, billID, idempotencyKey))
	if errors.Is(err, sqldb.ErrNoRows) {
		return billflow.LineItem{}, false, nil
	}
	if err != nil {
		return billflow.LineItem{}, false, karma.Format(err, "query line item by idempotency key")
	}
	return li, true, nil
}

// InsertLineItem takes a share lock on the bill row for the duration of the
// insert. CloseBill takes the exclusive lock on the same row, so the two
// cannot interleave: an insert either commits before the total is computed
// or finds the bill closed.
func (s store) InsertLineItem(ctx context.Context, item billflow.LineItem) (billflow.LineItem, error) {
	_, err := s.db.Exec(ctx, `
		INSERT INTO line_items (id, bill_id, description, amount_minor, currency, created_at, idempotency_key)
		SELECT $1, b.id, $3, $4, $5, $6, $7
		FROM bills b
		WHERE b.id = $2 AND b.status = 'open'
		FOR SHARE
		ON CONFLICT (bill_id, idempotency_key) DO NOTHING`,
		item.ID, item.BillID, item.Description, item.Amount, item.Currency, item.CreatedAt, item.IdempotencyKey)
	if isDataError(err) {
		return billflow.LineItem{}, karma.Format(errors.Join(billflow.ErrPermanent, err), "insert line item")
	}
	if err != nil {
		return billflow.LineItem{}, karma.Format(err, "insert line item")
	}

	stored, found, err := s.lineItemByKey(ctx, item.BillID, item.IdempotencyKey)
	if err != nil {
		return billflow.LineItem{}, karma.Format(err, "read back line item")
	}
	if !found {
		return billflow.LineItem{}, billflow.ErrBillNotOpen
	}
	return stored, nil
}

// isDataError reports whether Postgres refused the statement because of the
// data itself: SQLSTATE class 22 (data exception) or 23 (integrity
// constraint violation). Sending the same data again fails the same way.
func isDataError(err error) bool {
	var dbErr *sqldb.Error
	if !errors.As(err, &dbErr) {
		return false
	}
	return strings.HasPrefix(dbErr.DatabaseCode, "22") || strings.HasPrefix(dbErr.DatabaseCode, "23")
}

// CloseBill locks the bill row first and sums in a second statement. Under
// READ COMMITTED, the default isolation level this relies on, each statement
// sees what committed before it started, so the sum includes every insert
// that held the share lock ahead of this close.
func (s store) CloseBill(ctx context.Context, billID string, closedAt time.Time) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return karma.Format(err, "begin close")
	}
	defer func() { _ = tx.Rollback() }()

	var locked string
	err = tx.QueryRow(ctx, `SELECT id FROM bills WHERE id = $1 FOR UPDATE`, billID).Scan(&locked)
	if errors.Is(err, sqldb.ErrNoRows) {
		return karma.Format(errors.Join(billflow.ErrPermanent, errBillNotFound), "close bill %s", billID)
	}
	if err != nil {
		return karma.Format(err, "lock bill")
	}

	_, err = tx.Exec(ctx, `
		UPDATE bills
		SET status = 'closed',
		    closed_at = $2,
		    total_minor = (SELECT COALESCE(SUM(amount_minor), 0) FROM line_items WHERE bill_id = $1)
		WHERE id = $1 AND status = 'open'`,
		billID, closedAt)
	if err != nil {
		return karma.Format(err, "close bill")
	}
	if err := tx.Commit(); err != nil {
		return karma.Format(err, "commit close")
	}
	return nil
}
