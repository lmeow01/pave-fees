package money_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lmeow01/pave-fees/fees/money"
)

func TestParseCurrency(t *testing.T) {
	tests := []struct {
		in      string
		want    money.Currency
		wantErr bool
	}{
		{in: "USD", want: money.USD},
		{in: "GEL", want: money.GEL},
		{in: "usd", wantErr: true},
		{in: "EUR", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := money.ParseCurrency(tt.in)
			if tt.wantErr {
				require.ErrorIs(t, err, money.ErrUnsupportedCurrency)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseAmount(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    money.Amount
		wantErr bool
	}{
		{name: "zero", in: "0", want: 0},
		{name: "ten dollars in cents", in: "1000", want: 1000},
		{name: "largest allowed", in: "1000000000000", want: money.MaxAmount},
		{name: "above largest allowed", in: "1000000000001", wantErr: true},
		{name: "beyond int64", in: "99999999999999999999", wantErr: true},
		{name: "decimal point", in: "10.50", wantErr: true},
		{name: "negative", in: "-1", wantErr: true},
		{name: "explicit plus sign", in: "+1", wantErr: true},
		{name: "exponent", in: "1e3", wantErr: true},
		{name: "surrounding space", in: " 10", wantErr: true},
		{name: "empty", in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := money.ParseAmount(tt.in)
			if tt.wantErr {
				require.ErrorIs(t, err, money.ErrInvalidAmount)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAmountStringRoundTrips(t *testing.T) {
	got, err := money.ParseAmount(money.Amount(12012000).String())
	require.NoError(t, err)
	assert.Equal(t, money.Amount(12012000), got)
}
