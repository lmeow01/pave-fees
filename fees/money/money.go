// Package money represents monetary values as integer minor units of a
// currency (cents for USD, tetri for GEL), so no arithmetic or storage step
// ever touches a floating point number.
package money

import (
	"errors"
	"fmt"
	"strconv"
)

// Errors returned by the parsers, for matching with errors.Is.
var (
	ErrUnsupportedCurrency = errors.New("unsupported currency")
	ErrInvalidAmount       = errors.New("invalid amount")
)

// Currency is an ISO 4217 alphabetic code.
type Currency string

// Supported currencies. Both have two decimal places.
const (
	GEL Currency = "GEL"
	USD Currency = "USD"
)

// ParseCurrency accepts only the exact upper-case code of a supported currency.
func ParseCurrency(code string) (Currency, error) {
	switch c := Currency(code); c {
	case GEL, USD:
		return c, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedCurrency, code)
	}
}

// Amount is a count of minor units.
type Amount int64

// MaxAmount bounds a single amount so that a bill needs millions of
// maximum-size line items before its total could overflow an int64.
const MaxAmount Amount = 1_000_000_000_000

// ParseAmount parses the wire form of an amount: a string of decimal digits
// with no sign, separator or exponent, at most MaxAmount.
func ParseAmount(s string) (Amount, error) {
	if s == "" {
		return 0, fmt.Errorf("%w: empty", ErrInvalidAmount)
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%w: %q is not a whole number of minor units", ErrInvalidAmount, s)
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || Amount(n) > MaxAmount {
		return 0, fmt.Errorf("%w: %q exceeds the maximum of %d", ErrInvalidAmount, s, MaxAmount)
	}
	return Amount(n), nil
}

func (a Amount) String() string {
	return strconv.FormatInt(int64(a), 10)
}
