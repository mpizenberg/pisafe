package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"

	"github.com/mpizenberg/pisafe/internal/chatgpt"
)

// runLogin stores a provider credential on the Mac. Runs never receive it;
// the broker attaches it upstream on their behalf.
func runLogin(ctx context.Context, provider string, out io.Writer) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("pisafe login requires macOS")
	}
	if provider != "chatgpt" {
		return fmt.Errorf("unknown login provider %q; only chatgpt is supported", provider)
	}
	credential, err := chatgpt.Login(
		ctx,
		chatgpt.DefaultEndpoints(),
		out,
		func(url string) error {
			return exec.CommandContext(ctx, "/usr/bin/open", url).Run()
		},
	)
	if err != nil {
		return err
	}
	if err := chatgpt.NewKeychain().Save(ctx, credential); err != nil {
		return err
	}
	fmt.Fprintf(
		out,
		"Logged in to ChatGPT (account %s).\n"+
			"The OAuth tokens stay in your macOS Keychain; runs only ever see a\n"+
			"revocable per-run capability served by pisafe broker.\n",
		credential.AccountID,
	)
	return nil
}
