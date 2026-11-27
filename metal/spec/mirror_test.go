package spec

import (
	"path/filepath"
	"testing"
)

// mirrored are the functions of internal/controller whose control flow the
// protocol model (internal/controller/protocol_model_test.go) copies by hand,
// branch by branch, with their fingerprint as of the last time the model was
// brought in line.
//
// A change to one of them fails TestModelMirrorsTheCode. Then read the
// change, and bring the pass of the model that mirrors it in line, or
// decide that the change does not touch the protocol. Run TestProtocolModel,
// and write the new fingerprint here.
var mirrored = map[string]string{
	"HostClaimReconciler.Reconcile":     "f483186c1bc6",
	"HostClaimReconciler.finishRelease": "0988f8ac9564",
	"HostClaimReconciler.provision":     "c3c6afc9a724",
	"HostClaimReconciler.release":       "35999f23f29c",
	"HostOperationReconciler.Reconcile": "7f6da928f6a1",
}

// TestModelMirrorsTheCode fails when a function that the protocol model
// mirrors changed and the model was not looked at. Without it, the model
// goes on proving the protocol of an older version of the code.
func TestModelMirrorsTheCode(t *testing.T) {
	got := fingerprints(t, filepath.Join("..", "internal", "controller"))
	for name, want := range mirrored {
		have, ok := got[name]
		switch {
		case !ok:
			t.Errorf("%s is gone: update the protocol model and mirrored", name)
		case have != want:
			t.Errorf("%s changed since the protocol model last matched it. Bring its mirror in "+
				"internal/controller/protocol_model_test.go in line, run TestProtocolModel, "+
				"then set its fingerprint in mirrored to %q", name, have)
		}
	}
}
