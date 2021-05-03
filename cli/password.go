package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/term"

	"github.com/kopia/kopia/repo"
)

// TODO - remove those globals.
var (
	globalPassword          string // bound to the --password flag.
	globalPasswordFromToken string // set dynamically
)

func askForNewRepositoryPassword() (string, error) {
	for {
		p1, err := askPass("Enter password to create new repository: ")
		if err != nil {
			return "", errors.Wrap(err, "password entry")
		}

		p2, err := askPass("Re-enter password for verification: ")
		if err != nil {
			return "", errors.Wrap(err, "password verification")
		}

		if p1 != p2 {
			fmt.Println("Passwords don't match!")
		} else {
			return p1, nil
		}
	}
}

func askForExistingRepositoryPassword() (string, error) {
	p1, err := askPass("Enter password to open repository: ")
	if err != nil {
		return "", err
	}

	fmt.Println()

	return p1, nil
}

func (c *App) getPasswordFromFlags(ctx context.Context, isNew, allowPersistent bool) (string, error) {
	switch {
	case globalPasswordFromToken != "":
		// password extracted from connection token
		return globalPasswordFromToken, nil
	case globalPassword != "":
		// password provided via --password flag or KOPIA_PASSWORD environment variable
		return strings.TrimSpace(globalPassword), nil
	case isNew:
		// this is a new repository, ask for password
		return askForNewRepositoryPassword()
	case allowPersistent:
		// try fetching the password from persistent storage specific to the configuration file.
		pass, ok := repo.GetPersistedPassword(ctx, c.repositoryConfigFileName())
		if ok {
			return pass, nil
		}
	}

	// fall back to asking for existing password
	return askForExistingRepositoryPassword()
}

// askPass presents a given prompt and asks the user for password.
func askPass(prompt string) (string, error) {
	for i := 0; i < 5; i++ {
		fmt.Print(prompt)

		passBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			return "", errors.Wrap(err, "password prompt error")
		}

		fmt.Println()

		if len(passBytes) == 0 {
			continue
		}

		return string(passBytes), nil
	}

	return "", errors.New("can't get password")
}
