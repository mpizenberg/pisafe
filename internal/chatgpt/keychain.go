package chatgpt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const (
	keychainService = "pisafe"
	keychainAccount = "chatgpt"
)

// ErrNotLoggedIn distinguishes "no credential stored" from Keychain failures
// so callers can prompt for pisafe login instead of failing opaquely.
var ErrNotLoggedIn = errors.New("no ChatGPT login is stored; run: pisafe login chatgpt")

// Keychain persists the credential in the macOS login keychain through
// /usr/bin/security. The secret is written over security's interactive stdin
// so tokens never appear in process arguments, and it is base64-wrapped so
// the JSON survives security's whitespace-based command tokenizer.
type Keychain struct {
	execute func(ctx context.Context, stdin string, args ...string) (string, error)
}

func NewKeychain() Keychain {
	return Keychain{execute: runSecurity}
}

func (keychain Keychain) Save(ctx context.Context, credential Credential) error {
	if err := credential.validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(credential)
	if err != nil {
		return fmt.Errorf("encode chatgpt credential: %w", err)
	}
	command := fmt.Sprintf(
		"add-generic-password -U -s %s -a %s -w %s\n",
		keychainService,
		keychainAccount,
		base64.StdEncoding.EncodeToString(encoded),
	)
	if _, err := keychain.execute(ctx, command, "-i"); err != nil {
		return fmt.Errorf("store chatgpt credential in the keychain: %w", err)
	}
	return nil
}

func (keychain Keychain) Load(ctx context.Context) (Credential, error) {
	output, err := keychain.execute(
		ctx,
		"",
		"find-generic-password",
		"-s", keychainService,
		"-a", keychainAccount,
		"-w",
	)
	if err != nil {
		if strings.Contains(err.Error(), "could not be found") {
			return Credential{}, ErrNotLoggedIn
		}
		return Credential{}, fmt.Errorf("read chatgpt credential from the keychain: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(output))
	if err != nil {
		return Credential{}, fmt.Errorf("decode stored chatgpt credential: %w", err)
	}
	var credential Credential
	if err := json.Unmarshal(decoded, &credential); err != nil {
		return Credential{}, fmt.Errorf("parse stored chatgpt credential: %w", err)
	}
	if err := credential.validate(); err != nil {
		return Credential{}, fmt.Errorf("stored chatgpt credential is incomplete: %w", err)
	}
	return credential, nil
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
