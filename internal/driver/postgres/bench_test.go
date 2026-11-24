package postgres

import (
	"testing"

	"github.com/amrshadid/nopacs/internal/master/mastertest"
)

// BenchmarkKeyByID measures the first read of every request with a key.
func BenchmarkKeyByID(b *testing.B) { mastertest.BenchKeyByID(b, openTest) }

// BenchmarkGrantsByKey measures the second read of every request with a key.
func BenchmarkGrantsByKey(b *testing.B) { mastertest.BenchGrantsByKey(b, openTest) }

// BenchmarkRolesFor measures the access check's read of a key's roles.
func BenchmarkRolesFor(b *testing.B) { mastertest.BenchRolesFor(b, openTest) }

// BenchmarkAppendAudit measures the audit write every request ends with.
func BenchmarkAppendAudit(b *testing.B) { mastertest.BenchAppendAudit(b, openTest) }

// BenchmarkMarkKeyUsedNoWrite measures marking a busy key used within the
// minute, which writes nothing.
func BenchmarkMarkKeyUsedNoWrite(b *testing.B) { mastertest.BenchMarkKeyUsedNoWrite(b, openTest) }

// BenchmarkSetSettingByKey measures a change by a key, checked inside the
// change's transaction.
func BenchmarkSetSettingByKey(b *testing.B) { mastertest.BenchSetSettingByKey(b, openTest) }

// BenchmarkSetSettingByLocalOperator measures the same change by the local
// operator, whom no store checks.
func BenchmarkSetSettingByLocalOperator(b *testing.B) {
	mastertest.BenchSetSettingByLocalOperator(b, openTest)
}
