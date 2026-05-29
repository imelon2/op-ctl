package keymap

import (
	"strconv"
	"time"
)

// TimeMode is the `t`-cycled display mode for a unix-seconds column.
// Default is UTC since chain events are chain-time first.
type TimeMode int

const (
	TimeModeUTC   TimeMode = iota // 2026-05-29T10:14:32Z
	TimeModeLocal                 // 2026-05-29 19:14:32
	TimeModeUnix                  // 1748528072
)

const timeModeCount = 3

func (m TimeMode) Next() TimeMode {
	return (m + 1) % timeModeCount
}

// Format returns "" for unix==0 so missing values don't render 1970.
func (m TimeMode) Format(unix int64) string {
	if unix == 0 {
		return ""
	}
	switch m {
	case TimeModeUnix:
		return strconv.FormatInt(unix, 10)
	case TimeModeLocal:
		return time.Unix(unix, 0).Local().Format("2006-01-02 15:04:05")
	default:
		return time.Unix(unix, 0).UTC().Format("2006-01-02T15:04:05Z")
	}
}

// HeaderLabel returns "time (utc|local|unix)" for the column header.
func (m TimeMode) HeaderLabel() string {
	switch m {
	case TimeModeLocal:
		return "time (local)"
	case TimeModeUnix:
		return "time (unix)"
	default:
		return "time (utc)"
	}
}

func (m TimeMode) String() string {
	switch m {
	case TimeModeLocal:
		return "local"
	case TimeModeUnix:
		return "unix"
	default:
		return "utc"
	}
}
