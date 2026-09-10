package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/PlakarKorp/kloset/objects"
)

// TestSnapshotCaptureStatusFollowsTheRun pins the tag to the run's own verdict.
// It was the constant StatusComplete, so a run that recorded
// `capture_complete: false` in backup_information.json still tagged every
// snapshot complete (FR-012).
func TestSnapshotCaptureStatusFollowsTheRun(t *testing.T) {
	iops := NewInfrahubOps()
	if status := iops.snapshotCaptureStatus(); status != StatusComplete {
		t.Errorf("snapshotCaptureStatus() = %q, want %q for a run with nothing recorded against it", status, StatusComplete)
	}

	iops.recordIncompleteCapture(serviceNeo4j, "the capture produced no artifact")
	if status := iops.snapshotCaptureStatus(); status != StatusIncomplete {
		t.Errorf("snapshotCaptureStatus() = %q, want %q once a capture reported itself incomplete", status, StatusIncomplete)
	}
}

// TestDetermineGroupStatusReadsTheCaptureVerdict is the other half of FR-012 on
// the Plakar backend: a group whose components are all present is still not a
// restore point when the run that made it said its capture did not finish.
// Ranking on component presence alone is what offered one anyway.
func TestDetermineGroupStatusReadsTheCaptureVerdict(t *testing.T) {
	present := []SnapshotInfo{
		{Component: ComponentNeo4j},
		{Component: ComponentPostgres},
		{Component: ComponentMetadata},
	}
	components := []string{ComponentNeo4j, ComponentPostgres, ComponentMetadata}

	t.Run("a complete capture with every component is a restore point", func(t *testing.T) {
		group := &BackupGroupInfo{Components: components, Snapshots: present}
		if status := determineGroupStatus(group); status != StatusComplete {
			t.Errorf("determineGroupStatus() = %q, want %q", status, StatusComplete)
		}
	})

	t.Run("an incomplete capture is not, even with every component", func(t *testing.T) {
		group := &BackupGroupInfo{Components: components, Snapshots: present, captureIncomplete: true}
		if status := determineGroupStatus(group); status != StatusIncomplete {
			t.Errorf("determineGroupStatus() = %q, want %q: an incomplete capture must not be offered as a restore point", status, StatusIncomplete)
		}
	})

	t.Run("a missing component is still incomplete", func(t *testing.T) {
		group := &BackupGroupInfo{Components: components, Snapshots: present[:1]}
		if status := determineGroupStatus(group); status != StatusIncomplete {
			t.Errorf("determineGroupStatus() = %q, want %q", status, StatusIncomplete)
		}
	})
}

// TestDiscardPlakarComponentsRemovesWhatWasCommitted is T101. The components
// commit one at a time, so a run that stopped on the second one had left the
// first committed and listable — and warning about it was the whole of the
// response, where FR-012 requires the artefact not to survive the run.
func TestDiscardPlakarComponentsRemovesWhatWasCommitted(t *testing.T) {
	neo4j := objects.MAC{1, 2, 3, 4, 5, 6, 7, 8}
	postgres := objects.MAC{9, 10, 11, 12, 13, 14, 15, 16}
	committed := []componentBackup{
		{component: ComponentNeo4j, mac: neo4j},
		{component: ComponentPostgres, mac: postgres},
	}

	t.Run("every committed component is removed", func(t *testing.T) {
		removed := []objects.MAC{}
		remove := func(mac objects.MAC) error {
			removed = append(removed, mac)

			return nil
		}

		iops := NewInfrahubOps()
		if err := iops.discardPlakarComponents(remove, committed, "20260903_120000"); err != nil {
			t.Fatalf("discardPlakarComponents() = %v, want the removal to succeed", err)
		}
		if len(removed) != 2 || removed[0] != neo4j || removed[1] != postgres {
			t.Errorf("removed = %v, want both committed snapshots", removed)
		}
	})

	t.Run("a run that committed nothing removes nothing", func(t *testing.T) {
		called := false
		remove := func(objects.MAC) error {
			called = true

			return nil
		}

		iops := NewInfrahubOps()
		if err := iops.discardPlakarComponents(remove, nil, "20260903_120000"); err != nil {
			t.Fatalf("discardPlakarComponents() = %v, want no failure", err)
		}
		if called {
			t.Error("a snapshot was removed for a run that never committed one")
		}
	})

	// A removal that failed is reported rather than swallowed: the snapshot is
	// then still in the repository and the operator has to know which one.
	t.Run("a failed removal names the snapshot and does not stop the sweep", func(t *testing.T) {
		attempts := 0
		remove := func(mac objects.MAC) error {
			attempts++
			if mac == neo4j {
				return errors.New("packfile is locked")
			}

			return nil
		}

		iops := NewInfrahubOps()
		err := iops.discardPlakarComponents(remove, committed, "20260903_120000")
		if err == nil {
			t.Fatal("discardPlakarComponents() = nil, want the failed removal reported")
		}
		if attempts != 2 {
			t.Errorf("attempted %d removals, want both attempted before returning", attempts)
		}
		if !strings.Contains(err.Error(), ComponentNeo4j) || !strings.Contains(err.Error(), "may still be present") {
			t.Errorf("err = %v, want the component named and the artefact reported as possibly still present", err)
		}
		if strings.Contains(err.Error(), ComponentPostgres) {
			t.Errorf("err = %v, want only the component that could not be removed named", err)
		}
	})
}
