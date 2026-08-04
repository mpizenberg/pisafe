package zedsettings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// span is the byte range one value occupies in the settings file. Every edit is
// expressed as a splice over such a range, so whatever surrounds it survives
// unchanged.
type span struct {
	begin int
	end   int
}

type member struct {
	key   string
	value span
}

// skip advances past whitespace and comments. Zed's settings file is JSON with
// comments, and the ones the user wrote have to outlive every edit pisafe
// makes.
func skip(content []byte, at int) int {
	for at < len(content) {
		switch {
		case content[at] == ' ', content[at] == '\t', content[at] == '\n', content[at] == '\r':
			at++
		case bytes.HasPrefix(content[at:], []byte("//")):
			end := bytes.IndexByte(content[at:], '\n')
			if end < 0 {
				return len(content)
			}
			at += end + 1
		case bytes.HasPrefix(content[at:], []byte("/*")):
			end := bytes.Index(content[at+2:], []byte("*/"))
			if end < 0 {
				return len(content)
			}
			at += end + 4
		default:
			return at
		}
	}
	return at
}

// valueEnd reports the byte just past the value beginning at start.
func valueEnd(content []byte, start int) (int, error) {
	if start >= len(content) {
		return 0, errors.New("settings end where a value was expected")
	}
	switch content[start] {
	case '"':
		return stringEnd(content, start)
	case '{':
		_, end, err := objectAt(content, start)
		return end, err
	case '[':
		_, end, err := arrayAt(content, start)
		return end, err
	}
	at := start
	for at < len(content) && !delimits(content[at]) {
		at++
	}
	if at == start {
		return 0, fmt.Errorf("settings hold no value at byte %d", start)
	}
	return at, nil
}

// delimits reports whether a byte ends a number, true, false, or null, none of
// which say where they stop.
func delimits(character byte) bool {
	switch character {
	case ',', '}', ']', ' ', '\t', '\n', '\r', '/':
		return true
	}
	return false
}

func stringEnd(content []byte, start int) (int, error) {
	for at := start + 1; at < len(content); at++ {
		switch content[at] {
		case '\\':
			at++
		case '"':
			return at + 1, nil
		}
	}
	return 0, errors.New("settings hold an unterminated string")
}

// objectAt reads the members of the object beginning at start, reporting the
// byte just past its closing brace.
func objectAt(content []byte, start int) ([]member, int, error) {
	if start >= len(content) || content[start] != '{' {
		return nil, 0, fmt.Errorf("settings hold no object at byte %d", start)
	}
	members := []member{}
	at := skip(content, start+1)
	for at < len(content) && content[at] != '}' {
		if content[at] != '"' {
			return nil, 0, fmt.Errorf("settings hold no member name at byte %d", at)
		}
		keyEnd, err := stringEnd(content, at)
		if err != nil {
			return nil, 0, err
		}
		key, err := decodeString(content[at:keyEnd])
		if err != nil {
			return nil, 0, err
		}
		at = skip(content, keyEnd)
		if at >= len(content) || content[at] != ':' {
			return nil, 0, fmt.Errorf("settings hold no value for %q", key)
		}
		valueStart := skip(content, at+1)
		valueStop, err := valueEnd(content, valueStart)
		if err != nil {
			return nil, 0, err
		}
		members = append(members, member{key: key, value: span{valueStart, valueStop}})
		at = skip(content, valueStop)
		if at < len(content) && content[at] == ',' {
			at = skip(content, at+1)
		}
	}
	if at >= len(content) {
		return nil, 0, errors.New("settings hold an unterminated object")
	}
	return members, at + 1, nil
}

// arrayAt reads the elements of the array beginning at start, reporting the
// byte just past its closing bracket.
func arrayAt(content []byte, start int) ([]span, int, error) {
	if start >= len(content) || content[start] != '[' {
		return nil, 0, fmt.Errorf("settings hold no array at byte %d", start)
	}
	elements := []span{}
	at := skip(content, start+1)
	for at < len(content) && content[at] != ']' {
		end, err := valueEnd(content, at)
		if err != nil {
			return nil, 0, err
		}
		elements = append(elements, span{at, end})
		at = skip(content, end)
		if at < len(content) && content[at] == ',' {
			at = skip(content, at+1)
		}
	}
	if at >= len(content) {
		return nil, 0, errors.New("settings hold an unterminated array")
	}
	return elements, at + 1, nil
}

// lookup names one member of an object. A key written twice reads as its last
// value, which is what the settings parser Zed uses does with it.
func lookup(members []member, key string) (span, bool) {
	found := span{}
	present := false
	for _, candidate := range members {
		if candidate.key == key {
			found, present = candidate.value, true
		}
	}
	return found, present
}

func decodeString(encoded []byte) (string, error) {
	decoded := ""
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return "", fmt.Errorf("read a settings string: %w", err)
	}
	return decoded, nil
}

// quote renders one string as JSON. Only maps and channels can fail to marshal,
// so a string always produces the bytes this returns.
func quote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// lineIndent is the leading whitespace of the line a byte sits on, which is
// what inserted lines have to repeat to sit level with what is already there.
func lineIndent(content []byte, at int) string {
	begin := at
	for begin > 0 && content[begin-1] != '\n' {
		begin--
	}
	end := begin
	for end < at && (content[end] == ' ' || content[end] == '\t') {
		end++
	}
	return string(content[begin:end])
}

func splice(content []byte, begin, end int, text string) []byte {
	spliced := make([]byte, 0, len(content)+len(text))
	spliced = append(spliced, content[:begin]...)
	spliced = append(spliced, text...)
	return append(spliced, content[end:]...)
}
