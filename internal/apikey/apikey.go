// Package apikey configures the upstreams reached with a key the user was
// issued, rather than with a login pisafe drives. The key stays in the
// Keychain and is read only when the broker answers a request; what is on disk
// says which upstreams exist and nothing more.
package apikey

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mpizenberg/pisafe/internal/broker"
	"github.com/mpizenberg/pisafe/internal/keychain"
)

// The catalogs mirror the pinned Pi AI model lists for these providers, with
// the per-model api, provider, and baseUrl fields stripped so nothing a run
// reads can route around the broker.
//
//go:embed anthropic.json
var anthropicCatalog []byte

//go:embed openai.json
var openaiCatalog []byte

const recordVersion = 1

// builtin is an upstream pisafe knows the shape of. Its endpoint and model
// list belong to the release rather than to the login, so an upgrade that adds
// a model reaches a key that was stored months ago.
type builtin struct {
	upstream string
	api      string
	models   []byte
	// keyed is where the user gets the key, which is the one thing a login
	// prompt cannot work out for itself.
	keyed string
}

var builtins = map[string]builtin{
	"anthropic": {
		upstream: "https://api.anthropic.com",
		api:      broker.APIAnthropicMessages,
		models:   anthropicCatalog,
		keyed:    "https://console.anthropic.com/settings/keys",
	},
	"openai": {
		upstream: "https://api.openai.com",
		api:      broker.APIOpenAIResponses,
		models:   openaiCatalog,
		keyed:    "https://platform.openai.com/api-keys",
	},
}

// Builtin reports where to get a key for a provider pisafe already knows, and
// whether it knows it at all.
func Builtin(name string) (string, bool) {
	known, ok := builtins[name]
	return known.keyed, ok
}

// Record is what one API-key login is, apart from the key itself. A built-in
// provider records only its name, because everything else about it ships with
// pisafe. A custom endpoint has no release to belong to, so it carries its
// own endpoint, wire format, and model list.
type Record struct {
	Version int               `json:"version"`
	Name    string            `json:"name"`
	URL     string            `json:"url,omitempty"`
	API     string            `json:"api,omitempty"`
	Models  []json.RawMessage `json:"models,omitempty"`
}

// Custom reports whether this login is to an endpoint pisafe knows nothing
// about, which is what determines how much the record has to carry.
func (record Record) Custom() bool {
	_, known := builtins[record.Name]
	return !known
}

// Describe names the upstream a record reaches without reading its key.
func (record Record) Describe() string {
	if known, ok := builtins[record.Name]; ok {
		return known.upstream
	}
	return record.URL
}

type secretStore interface {
	Save(ctx context.Context, account string, secret []byte) error
	Load(ctx context.Context, account string) ([]byte, error)
	Delete(ctx context.Context, account string) error
}

// Store keeps the records beside the run records, and the keys they name in
// the Keychain. Every stored key is named by a record: that is what makes a
// login removable, so both writing and removing one are ordered to hold it
// even when interrupted.
type Store struct {
	root    string
	secrets secretStore
}

func NewStore(root string) Store {
	return Store{root: root, secrets: keychain.New()}
}

// Save records one login. The record goes first, so an interrupted login
// leaves a provider whose key is missing — which reports itself and is fixed
// by logging in again — rather than a key nothing can name or remove.
func (store Store) Save(ctx context.Context, record Record, key string) error {
	record.Version = recordVersion
	// A trailing slash would make every relayed path start with a doubled one.
	record.URL = strings.TrimSuffix(record.URL, "/")
	if err := record.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("the API key is empty")
	}
	if err := os.MkdirAll(store.root, 0o700); err != nil {
		return fmt.Errorf("create provider record directory: %w", err)
	}
	content, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode provider record: %w", err)
	}
	recordPath, err := store.path(record.Name)
	if err != nil {
		return err
	}
	if err := os.WriteFile(recordPath, append(content, '\n'), 0o600); err != nil {
		return fmt.Errorf("write provider record: %w", err)
	}
	return store.secrets.Save(ctx, record.Name, []byte(strings.TrimSpace(key)))
}

// Remove takes one login away entirely. The key goes first, for the same
// reason the record is written first: what is left over must always be
// something a later login or removal can still reach.
func (store Store) Remove(ctx context.Context, name string) error {
	recordPath, err := store.path(name)
	if err != nil {
		return err
	}
	if err := store.secrets.Delete(ctx, name); err != nil {
		return err
	}
	if err := os.Remove(recordPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove provider record: %w", err)
	}
	return nil
}

// Has reports whether a login is stored under this name, without asking
// whether it still describes something the relay could reach. A record made
// unusable by a hand edit or by a rule that has since tightened must still be
// removable, or the only way out is deleting files by hand.
func (store Store) Has(name string) (bool, error) {
	recordPath, err := store.path(name)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(recordPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read provider record: %w", err)
	}
	return true, nil
}

func (store Store) List() ([]Record, error) {
	entries, err := os.ReadDir(store.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list provider records: %w", err)
	}
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		record, err := store.get(name)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", name, err)
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Name < records[j].Name })
	return records, nil
}

// Providers turns the stored records into upstreams the broker can relay to.
// No key is read: what a run is told about a provider never includes its
// credential, so configuring a run touches no secret at all.
func (store Store) Providers() (broker.Catalog, error) {
	records, err := store.List()
	if err != nil {
		return nil, err
	}
	catalog := make(broker.Catalog, 0, len(records))
	for _, record := range records {
		provider, err := store.provider(record)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", record.Name, err)
		}
		catalog = append(catalog, provider)
	}
	return catalog, nil
}

// provider fills in from the release whatever a built-in name does not carry.
// The table wins over the record, so a record edited to point a known name
// somewhere else is ignored rather than obeyed.
func (store Store) provider(record Record) (broker.Provider, error) {
	upstream, api, models := record.URL, record.API, record.Models
	if known, ok := builtins[record.Name]; ok {
		upstream, api = known.upstream, known.api
		if err := json.Unmarshal(known.models, &models); err != nil {
			return broker.Provider{}, fmt.Errorf("parse embedded catalog: %w", err)
		}
	}
	parsed, err := url.Parse(upstream)
	if err != nil {
		return broker.Provider{}, fmt.Errorf("parse upstream URL: %w", err)
	}
	return broker.Provider{
		Name:        record.Name,
		Upstream:    parsed,
		API:         api,
		Models:      models,
		Credentials: newSource(record.Name, api, store.secrets),
	}, nil
}

func (store Store) get(name string) (Record, error) {
	path, err := store.path(name)
	if err != nil {
		return Record{}, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return Record{}, fmt.Errorf("read provider record: %w", err)
	}
	var record Record
	if err := json.Unmarshal(content, &record); err != nil {
		return Record{}, fmt.Errorf("decode provider record: %w", err)
	}
	if record.Version != recordVersion {
		return Record{}, fmt.Errorf("unsupported provider record version %d", record.Version)
	}
	if record.Name != name {
		return Record{}, errors.New("provider record identity mismatch")
	}
	if err := record.Validate(); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (store Store) path(name string) (string, error) {
	if err := broker.ValidateName(name); err != nil {
		return "", err
	}
	return filepath.Join(store.root, name+".json"), nil
}

// Validate reports whether this record describes an upstream the relay could
// route to. A login checks it before asking for the key, so a typo costs a
// retyped flag rather than a retyped secret.
func (record Record) Validate() error {
	if err := broker.ValidateName(record.Name); err != nil {
		return err
	}
	if !record.Custom() {
		return nil
	}
	if err := validateAPI(record.API); err != nil {
		return err
	}
	if err := validateUpstream(record.URL); err != nil {
		return err
	}
	if len(record.Models) == 0 {
		return errors.New("a custom endpoint must declare the models it serves")
	}
	return validateNoDoubledPath(record.URL, record.API)
}

// validateNoDoubledPath refuses an endpoint that already ends in the first
// segment of the path its client appends. Every OpenAI-compatible service
// documents its base URL with /v1 on the end, and pisafe adds the API path
// itself, so pasting the documented one would request /v1/v1/responses — a
// mistake nothing before the upstream's own 404 would report.
func validateNoDoubledPath(upstream, api string) error {
	parsed, err := url.Parse(upstream)
	if err != nil {
		return err
	}
	canonical := broker.Provider{API: api}.CanonicalPath()
	leading, _, _ := strings.Cut(strings.TrimPrefix(canonical, "/"), "/")
	if path.Base(strings.TrimSuffix(parsed.Path, "/")) != leading {
		return nil
	}
	return fmt.Errorf(
		"endpoint %q ends in %q, which pisafe appends itself for %s; give the URL without it",
		upstream,
		leading,
		api,
	)
}

// validateUpstream bounds where a custom login may send a key. Plain HTTP is
// allowed only to the loopback address, because anywhere else it puts the key
// on a network in clear — which is the exact exposure a broker holding the
// credential exists to prevent.
func validateUpstream(upstream string) error {
	parsed, err := url.Parse(upstream)
	if err != nil {
		return fmt.Errorf("invalid endpoint URL %q: %w", upstream, err)
	}
	if parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return fmt.Errorf("endpoint %q must be a plain scheme://host[/path] URL", upstream)
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if host := parsed.Hostname(); host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
		return fmt.Errorf(
			"endpoint %q would send the key over plain HTTP; use https, or reach it on localhost",
			upstream,
		)
	}
	return fmt.Errorf("endpoint %q must be http or https", upstream)
}

// validateAPI holds a custom endpoint to a wire format the relay knows how to
// route, because the API decides both the path a client requests and the path
// the broker calls upstream.
func validateAPI(api string) error {
	switch api {
	case broker.APIOpenAICompletions, broker.APIOpenAIResponses, broker.APIAnthropicMessages:
		return nil
	}
	return fmt.Errorf(
		"unknown API %q; expected %s, %s, or %s",
		api,
		broker.APIOpenAICompletions,
		broker.APIOpenAIResponses,
		broker.APIAnthropicMessages,
	)
}

// routingFields are the per-model fields that name an endpoint of their own. A
// run reads its models.json before it reads anything pisafe says, so a model
// carrying these would reach the provider directly and never touch the relay.
var routingFields = []string{"api", "provider", "baseUrl", "headers"}

// ParseModels reads a declared model list and returns it with any routing
// fields removed, along with how many models carried one. Pi's own provider
// data is the obvious thing to copy a list from and every entry in it carries
// them, so they are stripped rather than refused.
func ParseModels(content []byte) ([]json.RawMessage, int, error) {
	var models []map[string]json.RawMessage
	if err := json.Unmarshal(content, &models); err != nil {
		return nil, 0, fmt.Errorf("model list must be a JSON array of Pi model definitions: %w", err)
	}
	if len(models) == 0 {
		return nil, 0, errors.New("model list is empty")
	}
	parsed := make([]json.RawMessage, 0, len(models))
	stripped := 0
	for index, model := range models {
		var id string
		if err := json.Unmarshal(model["id"], &id); err != nil || id == "" {
			return nil, 0, fmt.Errorf("model %d has no id", index)
		}
		removed := false
		for _, field := range routingFields {
			if _, present := model[field]; present {
				delete(model, field)
				removed = true
			}
		}
		if removed {
			stripped++
		}
		encoded, err := json.Marshal(model)
		if err != nil {
			return nil, 0, fmt.Errorf("re-encode model %q: %w", id, err)
		}
		parsed = append(parsed, encoded)
	}
	return parsed, stripped, nil
}
