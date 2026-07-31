// Package keychain keeps pisafe's provider credentials in the macOS login
// keychain. Nothing stored here ever enters the VM: the broker reads a secret
// to authenticate one upstream request on a run's behalf, and the run itself
// only ever holds a revocable capability.
package keychain

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"unicode"
)

// service files every pisafe secret under one keychain service, so the account
// is what distinguishes one provider's credential from another's.
const service = "pisafe"

// ErrNotFound separates "nothing is stored" from a keychain that could not be
// read, which is what lets a caller tell a missing login from a broken one.
var ErrNotFound = errors.New("no credential is stored")

// Store reads and writes through /usr/bin/security. Secrets travel over
// security's interactive stdin so they never appear in process arguments, and
// are base64-wrapped so arbitrary bytes survive its whitespace-based
// tokenizer.
type Store struct {
	execute func(ctx context.Context, stdin string, args ...string) (string, error)
}

func New() Store {
	return Store{execute: runSecurity}
}

func (store Store) Save(ctx context.Context, account string, secret []byte) error {
	if err := validateAccount(account); err != nil {
		return err
	}
	command := fmt.Sprintf(
		"add-generic-password -U -s %s -a %s -w %s\n",
		service,
		account,
		base64.StdEncoding.EncodeToString(secret),
	)
	if _, err := store.execute(ctx, command, "-i"); err != nil {
		return fmt.Errorf("store the %s credential in the keychain: %w", account, err)
	}
	return nil
}

func (store Store) Load(ctx context.Context, account string) ([]byte, error) {
	if err := validateAccount(account); err != nil {
		return nil, err
	}
	output, err := store.execute(
		ctx, "", "find-generic-password", "-s", service, "-a", account, "-w",
	)
	if err != nil {
		if strings.Contains(err.Error(), "could not be found") {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read the %s credential from the keychain: %w", account, err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(output))
	if err != nil {
		return nil, fmt.Errorf("decode the stored %s credential: %w", account, err)
	}
	return decoded, nil
}

// Delete removes one credential. Having nothing to remove is not a failure:
// deleting a login also deletes what names it, and either order must be able
// to finish what the other started.
func (store Store) Delete(ctx context.Context, account string) error {
	if err := validateAccount(account); err != nil {
		return err
	}
	_, err := store.execute(ctx, "", "delete-generic-password", "-s", service, "-a", account)
	if err != nil && !strings.Contains(err.Error(), "could not be found") {
		return fmt.Errorf("remove the %s credential from the keychain: %w", account, err)
	}
	return nil
}

// validateAccount refuses anything security's tokenizer would read as more
// than one argument. The account is interpolated into a command written to
// security's stdin, so this is what stands between a provider name and an
// injected keychain operation.
func validateAccount(account string) error {
	if account == "" || strings.ContainsFunc(account, func(character rune) bool {
		return unicode.IsSpace(character) || !unicode.IsPrint(character)
	}) {
		return fmt.Errorf("invalid keychain account %q", account)
	}
	return nil
}

func runSecurity(ctx context.Context, stdin string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "/usr/bin/security", args...)
	if stdin != "" {
		command.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", errors.New(detail)
	}
	return stdout.String(), nil
}
