//go:build !clock

package clock

import "time"

// Now returns time.Now(). Without the build tag "clock" nothing moves it.
func Now() time.Time { return time.Now() }
