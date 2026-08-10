package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mpizenberg/pisafe/internal/keychain"
)

// ErrNotLoggedIn distinguishes "no credential stored" from Keychain failures
// so callers can prompt for pisafe login instead of failing opaquely.
var ErrNotLoggedIn = errors.New("no ChatGPT login is stored; run: pisafe login chatgpt")

// secretStore is the durable half of a login, kept as an interface so what a
// credential means stays here and where it is kept stays there.
type secretStore interface {
	Save(ctx context.Context, account string, secret []byte) error
	Has(ctx context.Context, account string) (bool, error)
	Load(ctx context.Context, account string) ([]byte, error)
	Delete(ctx context.Context, account string) error
}

// Keychain persists the OAuth credential as JSON under the provider's own
// name.
type Keychain struct {
	secrets secretStore
}

func NewKeychain() Keychain {
	return Keychain{secrets: keychain.New()}
}

func (store Keychain) Save(ctx context.Context, credential Credential) error {
	if err := credential.validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(credential)
	if err != nil {
		return fmt.Errorf("encode chatgpt credential: %w", err)
	}
	return store.secrets.Save(ctx, Name, encoded)
}

// Has reports whether a login is stored without reading it. It asks nothing
// about what is stored, so a credential that can no longer be parsed or
// refreshed is still something Forget can be asked to take away — and every
// command that only needs to know whether inference is configured stops short
// of the secret itself.
func (store Keychain) Has(ctx context.Context) (bool, error) {
	return store.secrets.Has(ctx, Name)
}

// Forget drops the login. The refresh token it held may still be live at the
// provider until it is used or expires, which is why the message a user gets
// says where to revoke it rather than claiming this did.
func (store Keychain) Forget(ctx context.Context) error {
	return store.secrets.Delete(ctx, Name)
}

func (store Keychain) Load(ctx context.Context) (Credential, error) {
	content, err := store.secrets.Load(ctx, Name)
	if errors.Is(err, keychain.ErrNotFound) {
		return Credential{}, ErrNotLoggedIn
	}
	if err != nil {
		return Credential{}, err
	}
	var credential Credential
	if err := json.Unmarshal(content, &credential); err != nil {
		return Credential{}, fmt.Errorf("parse stored chatgpt credential: %w", err)
	}
	if err := credential.validate(); err != nil {
		return Credential{}, fmt.Errorf("stored chatgpt credential is incomplete: %w", err)
	}
	return credential, nil
}
