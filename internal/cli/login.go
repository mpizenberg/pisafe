package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/mpizenberg/pisafe/internal/apikey"
	"github.com/mpizenberg/pisafe/internal/broker"
	"github.com/mpizenberg/pisafe/internal/chatgpt"
	"github.com/mpizenberg/pisafe/internal/providers"
)

var errLoginUsage = errors.New(
	"usage: pisafe login [chatgpt|anthropic|openai|NAME --url URL --api API --models FILE]",
)

// runLogin stores a provider credential on the Mac. Runs never receive one;
// the broker attaches it upstream on their behalf.
func runLogin(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("pisafe login requires macOS")
	}
	if len(args) == 0 {
		return listLogins(ctx, out)
	}
	name := args[0]
	if err := broker.ValidateName(name); err != nil {
		return err
	}
	if name == chatgpt.Name {
		if len(args) != 1 {
			return errLoginUsage
		}
		return loginChatGPT(ctx, out)
	}
	record, err := parseKeyedLogin(name, args[1:], out)
	if err != nil {
		return err
	}
	key, err := readKey(name, in, out)
	if err != nil {
		return err
	}
	if err := providers.Add(ctx, record, key); err != nil {
		return err
	}
	fmt.Fprintf(
		out,
		"Logged in to %s at %s.\n"+
			"The key stays in your macOS Keychain; runs only ever see a revocable\n"+
			"per-run capability served by pisafe broker. Restart the broker to relay it.\n",
		name,
		record.Describe(),
	)
	return nil
}

// parseKeyedLogin reads what a login needs beyond its key. A provider pisafe
// knows takes nothing else; anything else has to say where it is and what it
// speaks, because no release can know that for it.
func parseKeyedLogin(name string, args []string, out io.Writer) (apikey.Record, error) {
	record := apikey.Record{Name: name}
	var modelsPath string
	for index := 0; index < len(args); index += 2 {
		if index+1 == len(args) {
			return apikey.Record{}, fmt.Errorf("%s requires a value", args[index])
		}
		value := args[index+1]
		switch args[index] {
		case "--url":
			record.URL = value
		case "--api":
			record.API = value
		case "--models":
			modelsPath = value
		default:
			return apikey.Record{}, fmt.Errorf("unknown login option %q", args[index])
		}
	}
	if !record.Custom() {
		if record.URL != "" || record.API != "" || modelsPath != "" {
			return apikey.Record{}, fmt.Errorf(
				"%s is a provider pisafe already knows; it takes only a key", name,
			)
		}
		return record, nil
	}
	if record.URL == "" || record.API == "" || modelsPath == "" {
		return apikey.Record{}, fmt.Errorf(
			"%s is not a provider pisafe knows, so it needs --url, --api, and --models\n%s",
			name,
			errLoginUsage,
		)
	}
	content, err := os.ReadFile(modelsPath)
	if err != nil {
		return apikey.Record{}, fmt.Errorf("read the declared model list: %w", err)
	}
	models, stripped, err := apikey.ParseModels(content)
	if err != nil {
		return apikey.Record{}, err
	}
	if stripped > 0 {
		// Copying a list out of Pi's own provider data is the obvious way to
		// write one, and every entry there names an endpoint of its own.
		fmt.Fprintf(
			out,
			"Note: dropped the endpoint fields from %d model(s); a run reaches this\n"+
				"      provider through the broker and nowhere else.\n",
			stripped,
		)
	}
	record.Models = models
	return record, record.Validate()
}

func loginChatGPT(ctx context.Context, out io.Writer) error {
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

// readKey takes the key off stdin so it is never an argument, where every
// process on the Mac could read it and the shell would keep it in history.
// A terminal is asked to stop echoing first, so it does not survive on screen
// either.
func readKey(name string, in io.Reader, out io.Writer) (string, error) {
	if where, known := apikey.Builtin(name); known {
		fmt.Fprintf(out, "Create a key at %s\n", where)
	}
	if file, ok := in.(*os.File); ok && isTerminal(file) {
		fmt.Fprintf(out, "Paste the %s API key (not echoed): ", name)
		defer fmt.Fprintln(out)
		restore, err := silenceTerminal()
		if err != nil {
			return "", err
		}
		defer restore()
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || strings.TrimSpace(line) == "") {
		return "", fmt.Errorf("read the API key: %w", err)
	}
	key := strings.TrimSpace(line)
	if key == "" {
		return "", errors.New("no API key was given")
	}
	return key, nil
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// silenceTerminal turns off echo through stty, which is the whole of what a
// dependency-free password prompt needs on macOS.
func silenceTerminal() (func(), error) {
	saved, err := sttyRead("-g")
	if err != nil {
		return nil, err
	}
	if _, err := sttyRead("-echo"); err != nil {
		return nil, err
	}
	return func() { _, _ = sttyRead(saved) }, nil
}

func sttyRead(args ...string) (string, error) {
	command := exec.Command("/bin/stty", args...)
	command.Stdin = os.Stdin
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("configure the terminal for a hidden prompt: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func listLogins(ctx context.Context, out io.Writer) error {
	catalog, err := providers.Load(ctx)
	if err != nil {
		return err
	}
	if len(catalog) == 0 {
		fmt.Fprintf(out, "No provider is logged in.\n%s\n", errLoginUsage)
		return nil
	}
	fmt.Fprintln(out, "Runs are offered every one of these, and pick between them in Pi:")
	for _, line := range catalog.Describe() {
		fmt.Fprintf(out, "  %s\n", line)
	}
	fmt.Fprintln(out, "Take one away with pisafe logout NAME.")
	return nil
}

func runLogout(ctx context.Context, name string, out io.Writer) error {
	if err := providers.Remove(ctx, name); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed the %s login; runs started from now are not offered it.\n", name)
	if name == chatgpt.Name {
		fmt.Fprintln(
			out,
			"Its refresh token is gone from this Mac but may still be live at the\n"+
				"provider; revoke it there if that matters.",
		)
	}
	return nil
}
