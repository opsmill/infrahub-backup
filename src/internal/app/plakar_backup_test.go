package app

import (
	"errors"
	"testing"
)

// --redact destroys the live database, and the pre-redaction data only survives in
// the backup the same run is about to write. So the repository must be proven
// openable — for an encrypted repo, the passphrase must pass the canary — before
// the redaction runs. These tests pin that order; reversing it is how a mistyped
// passphrase destroyed the data and then failed to back it up.
func TestPrepareRepoBeforeRedactOrdering(t *testing.T) {
	t.Run("repository is prepared before the database is redacted", func(t *testing.T) {
		var calls []string
		err := prepareRepoBeforeRedact(true, true,
			func() error { calls = append(calls, "prepare"); return nil },
			func() error { calls = append(calls, "redact"); return nil },
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(calls) != 2 || calls[0] != "prepare" || calls[1] != "redact" {
			t.Fatalf("call order = %v, want [prepare redact]", calls)
		}
	})

	t.Run("a repository that cannot be prepared never reaches the redaction", func(t *testing.T) {
		prepareErr := errors.New("cannot open encrypted repository: incorrect passphrase")
		redacted := false
		err := prepareRepoBeforeRedact(true, true,
			func() error { return prepareErr },
			func() error { redacted = true; return nil },
		)
		if !errors.Is(err, prepareErr) {
			t.Fatalf("err = %v, want the preparation error", err)
		}
		if redacted {
			t.Fatal("the database was redacted even though the repository could not be opened")
		}
	})

	t.Run("--redact without --force refuses before touching either", func(t *testing.T) {
		prepared, redacted := false, false
		err := prepareRepoBeforeRedact(true, false,
			func() error { prepared = true; return nil },
			func() error { redacted = true; return nil },
		)
		if !errors.Is(err, errRedactRequiresForce) {
			t.Fatalf("err = %v, want errRedactRequiresForce", err)
		}
		if prepared || redacted {
			t.Fatalf("refusal did work anyway: prepared=%v redacted=%v", prepared, redacted)
		}
	})

	t.Run("without --redact the repository is still prepared", func(t *testing.T) {
		prepared, redacted := false, false
		if err := prepareRepoBeforeRedact(false, false,
			func() error { prepared = true; return nil },
			func() error { redacted = true; return nil },
		); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !prepared {
			t.Fatal("the repository was not prepared on the ordinary backup path")
		}
		if redacted {
			t.Fatal("the database was redacted without --redact")
		}
	})
}
