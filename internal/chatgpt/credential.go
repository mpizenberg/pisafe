// Package chatgpt drives the ChatGPT Plus/Pro subscription login and keeps
// its OAuth tokens on the Mac. The flow, endpoints, and request shapes mirror
// the pinned Pi AI client so the brokered upstream matches what an official
// login would produce; runs never see any of this state.
package chatgpt

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Credential is the durable outcome of one ChatGPT login. Access tokens are
// short-lived JWTs; the refresh token rotates on every refresh, so a stored
// credential is only valid until its next successful refresh is persisted.
type Credential struct {
	Access    string    `json:"access"`
	Refresh   string    `json:"refresh"`
	Expires   time.Time `json:"expires"`
	AccountID string    `json:"account_id"`
}

func (credential Credential) validate() error {
	if credential.Access == "" || credential.Refresh == "" ||
		credential.AccountID == "" || credential.Expires.IsZero() {
		return errors.New("chatgpt credential is missing required fields")
	}
	return nil
}

// extractAccountID reads the ChatGPT account ID from the access-token JWT.
// A token without it cannot be used against the subscription backend.
func extractAccountID(accessToken string) (string, error) {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return "", errors.New("access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", fmt.Errorf("decode access token payload: %w", err)
	}
	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("parse access token payload: %w", err)
	}
	if claims.Auth.ChatGPTAccountID == "" {
		return "", errors.New("access token carries no ChatGPT account ID")
	}
	return claims.Auth.ChatGPTAccountID, nil
}
