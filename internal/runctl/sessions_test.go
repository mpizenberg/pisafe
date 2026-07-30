package runctl

import (
	"strings"
	"testing"

	"github.com/mpizenberg/pisafe/internal/runstate"
)

func promotionCalls(calls []backendCall) []string {
	var promoted []string
	for _, call := range calls {
		if call.kind == "promote-sessions" {
			promoted = append(promoted, strings.Join(call.args, " "))
		}
	}
	return promoted
}

// TestStoppingPromotesTheRunsTranscripts is what makes an earlier run's history
// readable by a later one. Until it lands the session store stays empty and its
// overlay is indistinguishable from a per-run directory.
func TestStoppingPromotesTheRunsTranscripts(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{}
	stopped := stoppedByController(t, store, backend, testNPMCache)
	if stopped.LastError != "" {
		t.Fatalf("stop recorded %q", stopped.LastError)
	}
	promoted := promotionCalls(backend.calls)
	// Transcripts are filed under the project, and read out of the run that
	// wrote them: those two names are the whole of the operation.
	want := stopped.ProjectKey + " " + stopped.RunID
	if len(promoted) != 1 || promoted[0] != want {
		t.Fatalf("promotions = %#v, want one %q", promoted, want)
	}
}

// TestTranscriptsArePromotedByARunThatSharesNoCache separates sessions from
// caches: a cache is shared only because the repository declared it, while
// every run of every project has a transcript and never asked for one.
func TestTranscriptsArePromotedByARunThatSharesNoCache(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{}
	stopped := stoppedByController(t, store, backend)
	if stopped.LastError != "" {
		t.Fatalf("stop recorded %q", stopped.LastError)
	}
	if promoted := promotionCalls(backend.calls); len(promoted) != 1 {
		t.Fatalf("promotions = %#v, want one", promoted)
	}
}

// TestAFailedPromotionIsRecordedRatherThanFailingTheStop keeps a stop that
// worked reported as one. The transcript is still in the run's own storage, so
// nothing is lost until the run is discarded, and a stop that reported failure
// would only obscure that.
func TestAFailedPromotionIsRecordedRatherThanFailingTheStop(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{failAt: "promote-sessions"}
	stopped := stoppedByController(t, store, backend, testNPMCache)
	if stopped.State != runstate.StateStopped {
		t.Fatalf("stopped = %#v", stopped)
	}
	if !strings.Contains(stopped.LastError, "promote sessions failed") {
		t.Fatalf("last error = %q", stopped.LastError)
	}
	// A cache is published whatever the transcripts did, because the two halves
	// share nothing but the moment they run at.
	if !strings.Contains(callsString(backend.calls), "publish-snapshot") {
		t.Error("a failed promotion stopped the caches from publishing")
	}
}

// TestAFailedPublishStillPromotesTheTranscripts guards the halves against being
// short-circuited into each other. Promotion runs second, so an early return on
// the first failure would lose a transcript to a cache problem it shares nothing
// with.
func TestAFailedPublishStillPromotesTheTranscripts(t *testing.T) {
	store := runstate.NewStore(t.TempDir())
	backend := &fakeBackend{failAt: "publish-snapshot"}
	stopped := stoppedByController(t, store, backend, testNPMCache)
	if promoted := promotionCalls(backend.calls); len(promoted) != 1 {
		t.Fatalf("promotions = %#v, want one", promoted)
	}
	if !strings.Contains(stopped.LastError, "publish run caches") {
		t.Fatalf("last error = %q", stopped.LastError)
	}
}
