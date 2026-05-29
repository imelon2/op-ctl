package keymap

import (
	"math/big"
	"strings"
)

// FeeUnit is the `u`-cycled display denomination for wei amounts.
type FeeUnit int

const (
	UnitWei  FeeUnit = iota // raw uint256
	UnitGwei                // value / 1e9
	UnitEth                 // value / 1e18
)

const feeUnitCount = 3

func (u FeeUnit) Next() FeeUnit {
	return (u + 1) % feeUnitCount
}

func (u FeeUnit) Decimals() int {
	switch u {
	case UnitGwei:
		return 9
	case UnitEth:
		return 18
	default:
		return 0
	}
}

func (u FeeUnit) String() string {
	switch u {
	case UnitGwei:
		return "gwei"
	case UnitEth:
		return "eth"
	default:
		return "wei"
	}
}

// Format returns "" for nil and trims trailing zeros from the fraction.
func (u FeeUnit) Format(v *big.Int) string {
	if v == nil {
		return ""
	}
	d := u.Decimals()
	if d == 0 {
		return v.String()
	}
	return scaleDecimal(v, d)
}

func scaleDecimal(v *big.Int, decimals int) string {
	if decimals <= 0 {
		return v.String()
	}
	div := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	q, r := new(big.Int).QuoRem(v, div, new(big.Int))
	intPart := q.String()
	if r.Sign() == 0 {
		return intPart
	}
	frac := r.String()
	if pad := decimals - len(frac); pad > 0 {
		frac = strings.Repeat("0", pad) + frac
	}
	frac = strings.TrimRight(frac, "0")
	if frac == "" {
		return intPart
	}
	return intPart + "." + frac
}
