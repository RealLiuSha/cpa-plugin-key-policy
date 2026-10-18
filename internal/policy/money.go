package policy

import (
	"math"
	"strconv"
	"strings"
)

// Nano is an amount of US dollars in integer nanodollars (1 USD = 1e9). The
// ledger only ever adds Nano values, so totals stay exact and deterministic;
// floats appear only at the management API and in configuration.
type Nano int64

const nanoPerUSD = 1_000_000_000

// USDToNano rounds a configured dollar amount to the nearest nanodollar.
func USDToNano(usd float64) Nano {
	if math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0
	}
	return Nano(math.Round(usd * nanoPerUSD))
}

// USD converts for JSON responses, which report dollars as float64.
func (n Nano) USD() float64 {
	return float64(n) / nanoPerUSD
}

// FormatUSD prints the exact decimal value without exponent notation, for
// CSV export: 1500000000 → "1.5", 1234 → "0.000001234", 0 → "0".
func (n Nano) FormatUSD() string {
	negative := n < 0
	if negative {
		n = -n
	}
	whole := strconv.FormatInt(int64(n)/nanoPerUSD, 10)
	fraction := strings.TrimRight(strconv.FormatInt(int64(n)%nanoPerUSD+nanoPerUSD, 10)[1:], "0")
	var text string
	if fraction == "" {
		text = whole
	} else {
		text = whole + "." + fraction
	}
	if negative {
		return "-" + text
	}
	return text
}

// tokenCharge converts one class's tokens × USD-per-million price into
// nanodollars after the billing multiplier: tokens × price / 1e6 × 1e9 =
// tokens × price × 1e3. Inputs are bounded at the edges (tokens ≤ 2^31 per
// record, price ≤ 10000, multiplier ≤ 50), so each of the four priced
// classes stays below 1.1e18 and their sum below MaxInt64.
func tokenCharge(tokensTimesPrice float64, multiplier float64) Nano {
	return Nano(math.Round(tokensTimesPrice * 1_000 * multiplier))
}
