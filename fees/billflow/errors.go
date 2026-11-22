package billflow

import (
	"errors"
	"fmt"

	"go.temporal.io/sdk/temporal"

	"github.com/lmeow01/pave-fees/fees/money"
)

// Application error types let a rejection keep its meaning across the
// Temporal boundary, where Go error values do not survive.
const (
	errTypeBillClosed       = "BillClosed"
	errTypeCurrencyMismatch = "CurrencyMismatch"
	errTypeStoreFailure     = "StoreFailure"
)

func errBillClosed() error {
	return temporal.NewApplicationError("bill is closed", errTypeBillClosed)
}

func nonRetryableBillClosed(cause error) error {
	return temporal.NewNonRetryableApplicationError("bill is closed", errTypeBillClosed, cause)
}

func failOnceIfPermanent(err error) error {
	if errors.Is(err, ErrPermanent) {
		return temporal.NewNonRetryableApplicationError(err.Error(), errTypeStoreFailure, err)
	}
	return err
}

func errCurrencyMismatch(got, want money.Currency) error {
	return temporal.NewApplicationError(
		fmt.Sprintf("line item currency %s does not match bill currency %s", got, want),
		errTypeCurrencyMismatch,
	)
}

// IsBillClosed reports whether an update failed because the bill no longer
// accepts line items.
func IsBillClosed(err error) bool {
	return hasType(err, errTypeBillClosed)
}

// IsPermanentFailure reports whether an update failed because the store
// refused the write for good. Retrying the request cannot succeed.
func IsPermanentFailure(err error) bool {
	return hasType(err, errTypeStoreFailure)
}

// IsCurrencyMismatch reports whether an update was rejected because the line
// item is not in the bill's currency.
func IsCurrencyMismatch(err error) bool {
	return hasType(err, errTypeCurrencyMismatch)
}

func hasType(err error, errType string) bool {
	var appErr *temporal.ApplicationError
	return errors.As(err, &appErr) && appErr.Type() == errType
}
