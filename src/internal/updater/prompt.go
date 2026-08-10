package updater

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// ErrNonInteractive is returned when an update needs confirmation but stdin is
// not a terminal and --yes was not supplied.
var ErrNonInteractive = errors.New("cannot prompt for confirmation: re-run with --yes to update non-interactively")

// Testable seams for the confirmation prompt.
var (
	stdin            io.Reader = os.Stdin
	interactiveCheck           = defaultIsInteractive
)

func defaultIsInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// Proceed decides whether to apply an update. With assumeYes it proceeds
// silently; in an interactive session it prompts [y/N]; in a non-interactive
// session without assumeYes it returns ErrNonInteractive.
func Proceed(assumeYes bool, from, to string) (bool, error) {
	if assumeYes {
		return true, nil
	}
	if !interactiveCheck() {
		return false, ErrNonInteractive
	}
	return confirm(from, to)
}

// confirm prints the pending change and asks for [y/N] confirmation.
func confirm(from, to string) (bool, error) {
	fmt.Printf("Update %s → %s? [y/N]: ", from, to)
	reader := bufio.NewReader(stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}
