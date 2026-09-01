//go:build race

package protocol

import "testing"

func skipAllocationCheckUnderRace(t *testing.T) {
	t.Helper()
	t.Skip("allocation counts are not comparable under race instrumentation")
}
