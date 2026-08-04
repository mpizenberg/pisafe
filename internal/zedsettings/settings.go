// Package zedsettings adds and removes the saved SSH connections Zed needs in
// order to open a run. A run's alias resolves only through pisafe's per-run SSH
// config, and Zed hands the ssh binary nothing but what a saved connection
// carries, so without one the alias is a hostname nothing can resolve.
//
// The settings file belongs to the user and holds comments Zed's own settings
// editor preserves. Every edit here is therefore a byte splice over the one
// value it changes, never a re-encoding of the file.
package zedsettings

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// settingsLimit bounds what is read before an edit. Zed's settings are written
// by hand; anything this large is not them.
const settingsLimit = 1 << 20

// Connection is one saved server in Zed's settings: the host it names, and the
// pisafe config file that says how to reach it.
type Connection struct {
	Host       string
	ConfigFile string
}

// Path names the settings file Zed reads.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "zed", "settings.json"), nil
}

// Ensure gives Zed a connection it does not already have, reporting whether it
// wrote. A host already saved is left exactly as it is: Zed records the
// projects opened over a connection in the same entry, and that record is the
// user's, not pisafe's.
func Ensure(path string, connection Connection) (bool, error) {
	if connection.Host == "" || connection.ConfigFile == "" {
		return false, errors.New("a Zed connection needs a host and a config file")
	}
	content, mode, err := read(path)
	if errors.Is(err, fs.ErrNotExist) {
		content, mode, err = []byte("{\n}\n"), fs.FileMode(0o600), nil
	}
	if err != nil {
		return false, err
	}
	root := skip(content, 0)
	members, _, err := objectAt(content, root)
	if err != nil {
		return false, err
	}
	saved, present := lookup(members, "ssh_connections")
	if !present {
		indent := lineIndent(content, root) + "  "
		added := "\n" + indent + `"ssh_connections": [` + "\n" +
			render(connection, indent+"  ") + "\n" + indent + "]"
		if len(members) != 0 {
			added += ","
		}
		return true, write(path, splice(content, root+1, root+1, added), mode)
	}
	elements, _, err := arrayAt(content, saved.begin)
	if err != nil {
		return false, err
	}
	for _, element := range elements {
		host, err := connectionHost(content, element)
		if err != nil {
			return false, err
		}
		if host == connection.Host {
			return false, nil
		}
	}
	indent := lineIndent(content, saved.begin)
	added := "\n" + render(connection, indent+"  ")
	if len(elements) == 0 {
		added += "\n" + indent
	} else {
		added += ","
	}
	return true, write(path, splice(content, saved.begin+1, saved.begin+1, added), mode)
}

// Remove takes every connection to one host back out, reporting whether it
// wrote. A settings file that is not there is nothing to clean up.
func Remove(path string, host string) (bool, error) {
	content, mode, err := read(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	root := skip(content, 0)
	members, _, err := objectAt(content, root)
	if err != nil {
		return false, err
	}
	saved, present := lookup(members, "ssh_connections")
	if !present {
		return false, nil
	}
	elements, _, err := arrayAt(content, saved.begin)
	if err != nil {
		return false, err
	}
	doomed := []int{}
	for index, element := range elements {
		name, err := connectionHost(content, element)
		if err != nil {
			return false, err
		}
		if name == host {
			doomed = append(doomed, index)
		}
	}
	switch {
	case len(doomed) == 0:
		return false, nil
	case len(doomed) == len(elements):
		return true, write(path, splice(content, saved.begin, saved.end, "[]"), mode)
	}
	// Cutting from the last one back leaves every earlier offset describing the
	// bytes it was read from.
	for at := len(doomed) - 1; at >= 0; at-- {
		cut, err := cutRange(content, elements, doomed[at], saved.begin)
		if err != nil {
			return false, err
		}
		content = splice(content, cut.begin, cut.end, "")
	}
	return true, write(path, content, mode)
}

// cutRange is what one array element occupies together with the comma joining
// it to a neighbour, so removing it closes the array up as if the element had
// never been written. At least one element survives every cut made here, so a
// first element always has a comma after it.
func cutRange(content []byte, elements []span, index, arrayStart int) (span, error) {
	if index > 0 {
		return span{elements[index-1].end, elements[index].end}, nil
	}
	comma := skip(content, elements[index].end)
	if comma >= len(content) || content[comma] != ',' {
		return span{}, errors.New("settings hold no separator after a connection")
	}
	return span{arrayStart + 1, comma + 1}, nil
}

// connectionHost names the server one saved connection reaches. An element that
// is not an object, or an object naming no host, is not a connection pisafe
// wrote and is left alone.
func connectionHost(content []byte, element span) (string, error) {
	if content[element.begin] != '{' {
		return "", nil
	}
	members, _, err := objectAt(content, element.begin)
	if err != nil {
		return "", err
	}
	host, present := lookup(members, "host")
	if !present || content[host.begin] != '"' {
		return "", nil
	}
	return decodeString(content[host.begin:host.end])
}

// render writes one connection as an array element. The shape is the one Zed's
// own "Connect New Server" flow produces, empty project list included, so an
// entry pisafe wrote is the entry the user would have written by hand.
func render(connection Connection, indent string) string {
	return indent + "{\n" +
		indent + `  "host": ` + quote(connection.Host) + ",\n" +
		indent + `  "args": ["-F", ` + quote(connection.ConfigFile) + "],\n" +
		indent + `  "projects": []` + "\n" +
		indent + "}"
}

func read(path string) ([]byte, fs.FileMode, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > settingsLimit {
		return nil, 0, fmt.Errorf("%s is larger than %d bytes", path, settingsLimit)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	return content, info.Mode().Perm(), nil
}

// write replaces the settings file in one step. Zed rewrites the same file
// whenever a setting changes in the app, so an edit that appeared half-written
// would be read as the whole of it.
func write(path string, content []byte, mode fs.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create Zed settings directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".pisafe-settings-*")
	if err != nil {
		return fmt.Errorf("stage Zed settings: %w", err)
	}
	staged := temporary.Name()
	complete := false
	defer func() {
		temporary.Close()
		if !complete {
			os.Remove(staged)
		}
	}()
	if _, err := temporary.Write(content); err != nil {
		return fmt.Errorf("write Zed settings: %w", err)
	}
	if err := temporary.Chmod(mode); err != nil {
		return fmt.Errorf("restrict Zed settings: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("write Zed settings: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("write Zed settings: %w", err)
	}
	if err := os.Rename(staged, path); err != nil {
		return fmt.Errorf("replace Zed settings: %w", err)
	}
	complete = true
	return nil
}
