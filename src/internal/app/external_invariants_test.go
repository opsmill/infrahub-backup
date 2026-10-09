package app

import (
	"strings"
	"testing"
)

// TestAPreparationRefusesATargetThatDidNotComeFromTheGate is the construction
// witness on databaseTarget.
//
// The target is what the two preparers hold instead of re-deriving where a
// database lives and whether this run may write to it — and re-deriving would
// be a second answer to the question the gate exists to settle once. That makes
// the preparers' trust in their argument load-bearing, and it was held by
// convention: with four exported fields and nothing else, the literal below was
// a value any caller in the package could write, and it carries a zero Restore.
// Passed to the restore preparation it would have performed an unauthorised
// external restore, refused nowhere in the tool.
//
// No production caller ever wrote one, which is exactly the shape of the two
// defects this branch has already had: an invariant true until someone quietly
// broke it. So the verdict is now a field only databaseTargetWith sets.
func TestAPreparationRefusesATargetThatDidNotComeFromTheGate(t *testing.T) {
	// The value the review named, verbatim: external, a restore, no
	// authorisation.
	handBuilt := databaseTarget{
		Service:   serviceNeo4j,
		Location:  EndpointLocationExternal,
		Operation: databaseRestore,
	}

	t.Run("a restore", func(t *testing.T) {
		iops, _ := newExternalOps(t, "db-a")

		err := iops.prepareExternalRestoreWith((&recordingCaptureOps{}).ops(), handBuilt)
		if err == nil {
			t.Fatal("prepareExternalRestoreWith() = nil for a target that never passed the gate, want the refusal: this would be an unauthorised external restore (FR-009)")
		}
		if !strings.Contains(err.Error(), "did not come from the gate") {
			t.Errorf("err = %v, want it to say the target carries no verdict", err)
		}
		// Principle II's claim, on the refusal that comes before anything runs.
		if !strings.Contains(err.Error(), "No data was changed") {
			t.Errorf("err = %v, want it to state that nothing was changed", err)
		}
	})

	t.Run("a capture", func(t *testing.T) {
		iops, _ := newExternalOps(t, "db-a")

		capture := handBuilt
		capture.Operation = databaseCapture

		err := iops.prepareExternalSourceWith((&recordingCaptureOps{}).ops(), capture)
		if err == nil {
			t.Fatal("prepareExternalSourceWith() = nil for a target that never passed the gate, want the refusal: FR-002 forbids acting on a location that was never established")
		}
		if !strings.Contains(err.Error(), "did not come from the gate") {
			t.Errorf("err = %v, want it to say the target carries no verdict", err)
		}
	})

	// The other half: a target the gate did produce is accepted, so the witness
	// refuses the right thing rather than everything. Both preparations get
	// past the check and fail later, at the probe this double cannot answer —
	// which is what says the refusal above was the witness and not the
	// deployment.
	t.Run("a gated target is not refused for want of a witness", func(t *testing.T) {
		iops, _ := newExternalOps(t, "db-a")

		err := iops.prepareExternalSourceWith((&recordingCaptureOps{}).ops(), externalCaptureTarget(t, serviceNeo4j))
		if err != nil && strings.Contains(err.Error(), "did not come from the gate") {
			t.Errorf("err = %v, want a target from databaseTargetWith to carry its verdict", err)
		}
	})
}

// TestExternalRestoreAuthorisationIsItsChannel is FR-009 and FR-010 held by the
// type rather than by every constructor being careful.
//
// ExternalRestoreAuth carried an `Allowed bool` beside its Source, and the two
// answered one question between them: the refusal read only the bool, the audit
// line read only the source. `{Allowed: true}` was therefore representable — it
// authorised a destructive write into infrastructure the deployment does not
// manage and then recorded "no channel this run can name", losing the one fact
// FR-010 turns on, which is whether an operator was present. Every constructor
// in cli.go set both consistently; none had to.
//
// With the bool gone the state cannot be written down, and this pins what
// replaces it: the authorisation *is* the channel, and every value that
// authorises can name one.
func TestExternalRestoreAuthorisationIsItsChannel(t *testing.T) {
	for _, source := range []ExternalRestoreAuthSource{
		ExternalRestoreAuthAbsent,
		ExternalRestoreAuthFlag,
		ExternalRestoreAuthConfig,
	} {
		auth := ExternalRestoreAuth{Source: source}

		if want := source != ExternalRestoreAuthAbsent; auth.allowed() != want {
			t.Errorf("ExternalRestoreAuth{Source: %q}.allowed() = %t, want %t", source, auth.allowed(), want)
		}

		if auth.allowed() && strings.Contains(auth.describe(), "no channel") {
			t.Errorf("ExternalRestoreAuth{Source: %q} authorises a restore it cannot attribute (%q), which is the FR-010 fact the record exists for", source, auth.describe())
		}
		if !auth.allowed() && !strings.Contains(auth.describe(), "no channel") {
			t.Errorf("ExternalRestoreAuth{Source: %q}.describe() = %q, want it to name no channel", source, auth.describe())
		}
	}

	// The zero value is the unauthorised one, which is what makes a restore
	// path that never consults the authorisation unable to proceed by default.
	if (ExternalRestoreAuth{}).allowed() {
		t.Error("the zero ExternalRestoreAuth authorises a restore, want it to authorise nothing")
	}
}
