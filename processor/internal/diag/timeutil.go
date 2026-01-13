package diag

import (
	"sync"
	"time"
)

var (
	diagLoc     *time.Location
	diagLocOnce sync.Once
)

func diagLocation() *time.Location {
	diagLocOnce.Do(func() {
		loc, err := time.LoadLocation("Asia/Shanghai")
		if err != nil {
			loc = time.FixedZone("UTC+8", 8*3600)
		}
		diagLoc = loc
	})
	return diagLoc
}
