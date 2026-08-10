package keychain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeSecurity stands in for /usr/bin/security, answering the way it does and
// recording how it was invoked, which is the part that must not change: a
// secret passed as an argument would be visible to every process on the Mac.
type fakeSecurity struct {
	stored     map[string]string
	sawArgs    []string
	sawStdin   string
	deleteFail bool
}

func (fake *fakeSecurity) execute(
	_ context.Context,
	stdin string,
	args ...string,
) (string, error) {
	fake.sawArgs = args
	if stdin != "" {
		fake.sawStdin = stdin
	}
	if len(args) == 1 && args[0] == "-i" {
		fields := strings.Fields(stdin)
		if len(fields) != 8 || fields[0] != "add-generic-password" {
			return "", fmt.Errorf("unexpected interactive command %q", stdin)
		}
		fake.stored[fields[5]] = fields[7]
		return "", nil
	}
	switch args[0] {
	case "find-generic-password":
		secret, ok := fake.stored[args[4]]
		if !ok {
			return "", errors.New("The specified item could not be found in the keychain.")
		}
		return secret + "\n", nil
	case "delete-generic-password":
		if fake.deleteFail {
			return "", errors.New("keychain is locked")
		}
		if _, ok := fake.stored[args[4]]; !ok {
			return "", errors.New("The specified item could not be found in the keychain.")
		}
		delete(fake.stored, args[4])
		return "", nil
	}
	return "", fmt.Errorf("unexpected security invocation %v", args)
}

func TestSecretsTravelOverStdinAndSurviveTokenization(t *testing.T) {
	fake := &fakeSecurity{stored: map[string]string{}}
	store := Store{execute: fake.execute}
	ctx := context.Background()

	if _, err := store.Load(ctx, "chatgpt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty keychain err = %v", err)
	}
	// A secret with whitespace in it is what breaks a keychain write that
	// passes it as one word, so it is the shape worth storing.
	secret := []byte(`{"refresh": "tok en", "note": "two words"}`)
	if err := store.Save(ctx, "chatgpt", secret); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fake.sawStdin, "tok en") {
		t.Fatalf("secret was not wrapped: %q", fake.sawStdin)
	}
	for _, arg := range fake.sawArgs {
		if strings.Contains(arg, "refresh") {
			t.Fatalf("secret reached the argument list: %v", fake.sawArgs)
		}
	}
	loaded, err := store.Load(ctx, "chatgpt")
	if err != nil {
		t.Fatal(err)
	}
	if string(loaded) != string(secret) {
		t.Fatalf("loaded = %q", loaded)
	}

	// One account's credential is not another's.
	if err := store.Save(ctx, "anthropic", []byte("sk-other")); err != nil {
		t.Fatal(err)
	}
	if loaded, err := store.Load(ctx, "anthropic"); err != nil || string(loaded) != "sk-other" {
		t.Fatalf("loaded = %q err = %v", loaded, err)
	}
	if err := store.Delete(ctx, "anthropic"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, "anthropic"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted credential err = %v", err)
	}
	if loaded, err := store.Load(ctx, "chatgpt"); err != nil || string(loaded) != string(secret) {
		t.Fatalf("deleting one account disturbed another: %q %v", loaded, err)
	}
	// Removing what is already gone finishes the job a half-done removal left.
	if err := store.Delete(ctx, "anthropic"); err != nil {
		t.Fatal(err)
	}
	fake.deleteFail = true
	if err := store.Delete(ctx, "chatgpt"); err == nil {
		t.Fatal("a keychain that refused the delete was reported as success")
	}
}

// Every command that only has to know whether inference is configured asks this
// question, and asking it must not read the secret: security hands one over
// only when it is told to print one.
func TestHasAnswersWithoutAskingForTheSecret(t *testing.T) {
	fake := &fakeSecurity{stored: map[string]string{}}
	store := Store{execute: fake.execute}
	ctx := context.Background()

	stored, err := store.Has(ctx, "chatgpt")
	if err != nil || stored {
		t.Fatalf("stored = %v, err = %v", stored, err)
	}
	if err := store.Save(ctx, "chatgpt", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	stored, err = store.Has(ctx, "chatgpt")
	if err != nil || !stored {
		t.Fatalf("stored = %v, err = %v", stored, err)
	}
	for _, arg := range fake.sawArgs {
		if arg == "-w" {
			t.Fatalf("the existence check asked for the secret: %v", fake.sawArgs)
		}
	}
	if _, err := store.Has(ctx, "two words"); err == nil {
		t.Error("an account that would tokenize into more arguments was accepted")
	}
}

// An account name reaches security inside a command written to its stdin, so a
// name carrying whitespace would be read as further arguments.
func TestAnAccountThatWouldTokenizeIntoMoreArgumentsIsRefused(t *testing.T) {
	fake := &fakeSecurity{stored: map[string]string{}}
	store := Store{execute: fake.execute}
	ctx := context.Background()
	for _, account := range []string{
		"", "two words", "trailing\n-w", "tab\there", "bell\a",
	} {
		if err := store.Save(ctx, account, []byte("secret")); err == nil {
			t.Errorf("saved under %q", account)
		}
		if _, err := store.Load(ctx, account); err == nil {
			t.Errorf("loaded from %q", account)
		}
		if err := store.Delete(ctx, account); err == nil {
			t.Errorf("deleted %q", account)
		}
	}
	if len(fake.stored) != 0 {
		t.Fatalf("stored = %#v", fake.stored)
	}
}
