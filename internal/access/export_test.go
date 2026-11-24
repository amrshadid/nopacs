package access

import (
	"sync/atomic"

	"github.com/amrshadid/nopacs/internal/secret"
)

// CountVerifications replaces a's comparison of secrets with one that counts
// its calls and then compares as before, and returns the count so far. It is
// the seam a test uses to show that a key with an unknown id costs the same
// comparison as a wrong secret.
func CountVerifications(a *Access) func() int {
	var n atomic.Int64
	a.seams.verify = func(sec string, stored []byte) bool {
		n.Add(1)
		return secret.VerifyAPISecret(sec, stored)
	}
	return func() int { return int(n.Load()) }
}

// WaitForWorkersAfterCancel makes a's Close, when its deadline passes, wait
// after canceling the workers' writes until both workers have returned,
// before it counts what they held. A worker that honors the cancel then
// settles what it held first: the order in which a write cut short by Close
// would be counted twice, or in another count, if Close counted wrongly.
func WaitForWorkersAfterCancel(a *Access) {
	a.seams.afterCancel = func() {
		<-a.marksDone
		<-a.auditDone
	}
}

// ExpiryRounds returns how many rounds a's expiry worker has finished, so a
// test can wait for rounds instead of for time.
func ExpiryRounds(a *Access) int64 { return a.expiry.rounds.Load() }
