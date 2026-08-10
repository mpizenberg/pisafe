package guestcall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// DocumentLimit bounds every JSON document that crosses between the controller
// and the helper, in either direction. It is far above anything either side
// legitimately produces and far below what would make a reader allocate without
// bound.
const DocumentLimit = int64(1 << 20)

// Decode reads one document crossing that boundary: bounded, whole, and exact.
// Unknown fields and trailing data are refused, so neither side acts on
// something the other did not mean to send, and a run cannot smuggle anything
// past what the controller expects to receive.
func Decode[Document any](in io.Reader, subject string) (Document, error) {
	var document Document
	content, err := io.ReadAll(io.LimitReader(in, DocumentLimit+1))
	if err != nil {
		return document, fmt.Errorf("read %s: %w", subject, err)
	}
	if int64(len(content)) > DocumentLimit {
		return document, fmt.Errorf("%s exceeds size limit", subject)
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode %s: %w", subject, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return document, fmt.Errorf("%s contains trailing data", subject)
		}
		return document, fmt.Errorf("decode %s trailer: %w", subject, err)
	}
	return document, nil
}
