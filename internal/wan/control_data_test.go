package wan

import (
	"testing"

	"dissident/internal/control"
)

// Compile-time proof that Manager implements the control plane's Data method
// set exactly (Snapshot, actions, subscriptions, cycle state).
var _ control.Data = (*Manager)(nil)

func TestControlDataInterface(t *testing.T) {
	// The assertion above is the test; keep a runtime test so it is executed
	// and reported alongside the rest of the package.
	var data control.Data = New(Options{})
	if data == nil {
		t.Fatal("manager does not satisfy control.Data")
	}
}
