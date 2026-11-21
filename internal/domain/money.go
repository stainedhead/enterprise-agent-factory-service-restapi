package domain

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

var moneyRE = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,11})(\.[0-9]{1,6})?$`)

// microsPerUSD is the scale of Money: one millionth of a dollar.
const microsPerUSD = 1_000_000

// Money is an amount in USD held as micro-dollars (API contract §1: decimal string, at most 6
// fraction digits, never a float).
type Money struct{ micros int64 }

// ParseMoney parses a decimal string such as "1234.50".
func ParseMoney(field, s string) (Money, error) {
	if !moneyRE.MatchString(s) {
		return Money{}, Validation(FieldError{Field: field, Reason: "must be a decimal string with at most 6 fraction digits"})
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return Money{}, Validation(FieldError{Field: field, Reason: "not a decimal"})
	}
	r.Mul(r, big.NewRat(microsPerUSD, 1))
	return Money{micros: r.Num().Int64()}, nil
}

// MustMoney parses s and panics on error; for constants and tests.
func MustMoney(s string) Money {
	m, err := ParseMoney("amount", s)
	if err != nil {
		panic(err)
	}
	return m
}

// MoneyFromMicros returns an amount of n micro-dollars.
func MoneyFromMicros(n int64) Money { return Money{micros: n} }

// Micros returns the amount in micro-dollars.
func (m Money) Micros() int64 { return m.micros }

// Add returns m + o.
func (m Money) Add(o Money) Money { return Money{micros: m.micros + o.micros} }

// Sub returns m - o.
func (m Money) Sub(o Money) Money { return Money{micros: m.micros - o.micros} }

// Cmp compares m and o: -1, 0 or +1.
func (m Money) Cmp(o Money) int {
	switch {
	case m.micros < o.micros:
		return -1
	case m.micros > o.micros:
		return 1
	}
	return 0
}

// IsNegative reports whether m < 0.
func (m Money) IsNegative() bool { return m.micros < 0 }

// String formats m with at least two and at most six fraction digits ("12.50", "0.000123").
func (m Money) String() string {
	n := m.micros
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	frac := strings.TrimRight(fmt.Sprintf("%06d", n%microsPerUSD), "0")
	for len(frac) < 2 {
		frac += "0"
	}
	return fmt.Sprintf("%s%d.%s", sign, n/microsPerUSD, frac)
}

// MarshalText encodes m as a decimal string, so JSON carries a string, never a number.
func (m Money) MarshalText() ([]byte, error) { return []byte(m.String()), nil }

// UnmarshalText parses a decimal string.
func (m *Money) UnmarshalText(b []byte) error {
	v, err := ParseMoney("amount", string(b))
	if err != nil {
		return err
	}
	*m = v
	return nil
}
