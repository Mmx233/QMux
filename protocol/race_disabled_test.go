//go:build !race

package protocol

import "testing"

func skipAllocationCheckUnderRace(t *testing.T) {
	t.Helper()
}
