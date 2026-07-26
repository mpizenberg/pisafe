package gitstage

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Identity is the author a run commits as. A run is given the identity the
// user already commits with in the source repository, because the work an
// agent does there is theirs.
type Identity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

var ErrNoIdentity = errors.New(
	"no Git identity is configured, so commits made inside a run would fail; " +
		"set one with git config --global user.name and git config --global user.email",
)

// identityFieldLimit keeps a pathological configuration value from crossing the
// boundary; real names and addresses are far shorter.
const identityFieldLimit = 256

// ResolveIdentity reads the identity Git would use for a commit made in this
// repository, so repository-local and conditional configuration wins exactly
// as it does outside a run.
func ResolveIdentity(ctx context.Context, repository string) (Identity, error) {
	name, err := configValue(ctx, repository, "user.name")
	if err != nil {
		return Identity{}, err
	}
	email, err := configValue(ctx, repository, "user.email")
	if err != nil {
		return Identity{}, err
	}
	if name == "" || email == "" {
		return Identity{}, ErrNoIdentity
	}
	identity := Identity{Name: name, Email: email}
	return identity, identity.Validate()
}

// InstallIdentity records an identity in a Git configuration file, creating it
// if needed. Git writes the values itself, so no name or address can rewrite
// the surrounding configuration.
func InstallIdentity(ctx context.Context, configPath string, identity Identity) error {
	if err := identity.Validate(); err != nil {
		return err
	}
	for _, setting := range []struct{ key, value string }{
		{"user.name", identity.Name},
		{"user.email", identity.Email},
	} {
		if err := gitRun(
			ctx,
			"",
			nil,
			nil,
			"config", "--file", configPath, setting.key, setting.value,
		); err != nil {
			return fmt.Errorf("set %s: %w", setting.key, err)
		}
	}
	return nil
}

func (identity Identity) Validate() error {
	if err := validateIdentityField("user.name", identity.Name); err != nil {
		return err
	}
	return validateIdentityField("user.email", identity.Email)
}

func validateIdentityField(key, value string) error {
	switch {
	case value == "":
		return fmt.Errorf("%s is empty", key)
	case len(value) > identityFieldLimit:
		return fmt.Errorf("%s exceeds %d bytes", key, identityFieldLimit)
	case strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return fmt.Errorf("%s contains control characters", key)
	}
	return nil
}

// configValue reports an unset key as an empty value, which Git signals with
// exit code 1 rather than an error message.
func configValue(ctx context.Context, repository, key string) (string, error) {
	value, err := gitOutput(ctx, repository, "config", "--get", key)
	switch {
	case err == nil:
		return value, nil
	case isExitCode(err, 1):
		return "", nil
	default:
		return "", fmt.Errorf("read %s: %w", key, err)
	}
}
