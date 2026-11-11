package agent

import "time"

func testTime(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.Local) }
