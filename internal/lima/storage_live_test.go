package lima_test

import (
	"context"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/mpizenberg/pisafe/internal/lima"
	"github.com/mpizenberg/pisafe/internal/runcontainer"
)

// liveCacheKeys are what a declared cache's inputs would hash to. They are
// fixed here because this test seeds the project store directly rather than
// running the tool that would fill it.
const (
	liveOlderKey   = "1111111111111111"
	liveNewerKey   = "2222222222222222"
	liveMissingKey = "3333333333333333"
)

func liveCache(key string) runcontainer.CacheMount {
	return runcontainer.CacheMount{
		Name: "npm",
		Env:  []string{"npm_config_cache"},
		Key:  key,
	}
}

func liveProject(t *testing.T, transport lima.Transport, name string) string {
	t.Helper()
	projectKey := name + "-" + time.Now().UTC().Format("20060102150405")
	if err := transport.EnsureProjectStorage(context.Background(), projectKey); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := transport.RemoveProjectStorage(context.Background(), projectKey); err != nil {
			t.Errorf("remove live project storage: %v", err)
		}
	})
	return projectKey
}

// seedSnapshot publishes one generation the way slice 4 will, with a fixed
// modification time so recency is decided by the test rather than by how fast
// it runs.
func seedSnapshot(t *testing.T, ctx context.Context, namespace, key, content, stamp string) {
	t.Helper()
	directory := namespace + "/" + key
	runLive(t, ctx, "podman", "unshare", "sh", "-ec",
		"install -d -m 0700 -o 1000 -g 1000 "+namespace+" "+directory+
			" && printf %s "+content+" > "+directory+"/shared.txt"+
			" && chown 1000:1000 "+directory+"/shared.txt"+
			" && touch -d "+stamp+" "+directory)
}

// TestLiveCacheSnapshotsAreSelectedByKeyThenRecency covers the whole point of
// keying a namespace: an exact match restores what this run's inputs produced
// before, and a miss restores the previous generation rather than nothing, so
// a changed lockfile costs a delta instead of a cold fetch.
func TestLiveCacheSnapshotsAreSelectedByKeyThenRecency(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to test the dedicated VM")
	}
	ensureLiveVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	transport := lima.NewTransport()
	projectKey := liveProject(t, transport, "liveselect")
	namespace := runcontainer.
		DefaultSpec("seed", projectKey, liveImageID(t)).
		CacheNamespacePath("npm")

	// An empty namespace is the first run of a project, and must select
	// nothing rather than fail.
	selected, err := transport.SelectCacheSnapshots(ctx, projectKey, []runcontainer.CacheMount{
		liveCache(liveMissingKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	if selected[0].Snapshot != "" {
		t.Errorf("an unpublished namespace selected %q", selected[0].Snapshot)
	}

	seedSnapshot(t, ctx, namespace, liveOlderKey, "older", "@1000000000")
	seedSnapshot(t, ctx, namespace, liveNewerKey, "newer", "@2000000000")

	for name, request := range map[string]struct{ key, want string }{
		"exact match on the older generation": {liveOlderKey, liveOlderKey},
		"exact match on the newer generation": {liveNewerKey, liveNewerKey},
		"no match falls back to the newest":   {liveMissingKey, liveNewerKey},
	} {
		selected, err := transport.SelectCacheSnapshots(ctx, projectKey, []runcontainer.CacheMount{
			liveCache(request.key),
		})
		if err != nil {
			t.Fatal(err)
		}
		if selected[0].Snapshot != request.want {
			t.Errorf("%s: selected %q, want %q", name, selected[0].Snapshot, request.want)
		}
	}
}

// TestLivePublishedGenerationsAreImmutableAndDisposable covers the second half
// of sharing. What a run fetched has to reach the next run, the generation it
// started from has to survive that untouched, and a reset has to be able to
// throw all of it away — that last one is what makes the cache disposable in
// practice rather than in principle.
func TestLivePublishedGenerationsAreImmutableAndDisposable(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to test the dedicated VM")
	}
	imageID := liveImageID(t)
	ensureLiveVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	transport := lima.NewTransport()
	projectKey := liveProject(t, transport, "livepublish")
	runID := "livepublish-" + time.Now().UTC().Format("20060102150405")
	namespace := runcontainer.
		DefaultSpec("seed", projectKey, imageID).
		CacheNamespacePath("npm")
	seedSnapshot(t, ctx, namespace, liveOlderKey, "restored", "@1000000000")

	// The run's inputs hash to a generation nobody published, so it restores
	// the one that exists and publishes under its own key.
	selected, err := transport.SelectCacheSnapshots(ctx, projectKey, []runcontainer.CacheMount{
		liveCache(liveNewerKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	if selected[0].Snapshot != liveOlderKey {
		t.Fatalf("restored %q, want the only published generation", selected[0].Snapshot)
	}
	spec := runcontainer.DefaultSpec(runID, projectKey, imageID)
	spec.Caches = selected
	if err := transport.CreateRunStorage(ctx, runID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		runLive(t, context.Background(), "podman", "rm", "--force", "--ignore", spec.ContainerName())
		if err := transport.RemoveRunStorage(context.Background(), runID); err != nil {
			t.Errorf("remove live run storage: %v", err)
		}
	}()
	if err := transport.PrepareRunLayout(ctx, runID, spec.Caches); err != nil {
		t.Fatal(err)
	}
	runArgs, err := spec.RunArgs()
	if err != nil {
		t.Fatal(err)
	}
	runArgs = append(runArgs[:len(runArgs)-2:len(runArgs)-2], "sleep", "infinity")
	if _, err := transport.Execute(ctx, nil, append([]string{"podman"}, runArgs...)...); err != nil {
		t.Fatal(err)
	}
	inContainer(t, ctx, transport, spec, "printf fetched > /cache/npm/fetched.txt")
	// Publishing reads the merged view, so a deletion the run made has to be
	// absent from the generation rather than reappear from the lower.
	inContainer(t, ctx, transport, spec, "rm /cache/npm/shared.txt")
	if _, err := transport.Execute(
		ctx, nil, "podman", "rm", "--force", spec.ContainerName(),
	); err != nil {
		t.Fatal(err)
	}

	if err := transport.PublishCacheSnapshot(ctx, spec, spec.Caches[0]); err != nil {
		t.Fatal(err)
	}
	published := namespace + "/" + liveNewerKey
	if got := runLive(t, ctx, "podman", "unshare", "ls", published); got != "fetched.txt" {
		t.Errorf("published generation = %q, want only what the run left", got)
	}
	// The generation the run started from is a lower some other run may still
	// have mounted, so restoring it may never have written to it.
	if got := runLive(t, ctx, "podman", "unshare", "ls", namespace+"/"+liveOlderKey); got != "shared.txt" {
		t.Errorf("restored generation = %q after a run published over it", got)
	}
	// It is also the newest by publication time, which is what the next run
	// with an unknown key falls back to.
	next, err := transport.SelectCacheSnapshots(ctx, projectKey, []runcontainer.CacheMount{
		liveCache(liveMissingKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	if next[0].Snapshot != liveNewerKey {
		t.Errorf("next run falls back to %q, want the generation just published", next[0].Snapshot)
	}

	// Eviction keeps what a live run may still mount whatever its age, and
	// drops the rest.
	if err := transport.EvictCacheSnapshots(
		ctx, projectKey, "npm", 1, []string{liveOlderKey},
	); err != nil {
		t.Fatal(err)
	}
	if got := runLive(t, ctx, "podman", "unshare", "ls", namespace); got != liveOlderKey+"\n"+liveNewerKey {
		t.Errorf("namespace = %q after keeping one generation and one mounted lower", got)
	}
	if err := transport.EvictCacheSnapshots(ctx, projectKey, "npm", 1, nil); err != nil {
		t.Fatal(err)
	}
	if got := runLive(t, ctx, "podman", "unshare", "ls", namespace); got != liveNewerKey {
		t.Errorf("namespace = %q after keeping one generation", got)
	}

	if err := transport.ResetProjectCache(ctx, projectKey); err != nil {
		t.Fatal(err)
	}
	if got := runLive(t, ctx, "podman", "unshare", "ls", spec.ProjectPath()+"/cache"); got != "" {
		t.Errorf("cache = %q after a reset", got)
	}
	// Reset is for the disposable half of the project store, so the session
	// store has to still be there for slice 5 to promote into.
	runLive(t, ctx, "podman", "unshare", "test", "-d", spec.ProjectPath()+"/sessions")
}

// TestLiveProjectLayersAreSharedToReadAndPrivateToWrite runs two containers of
// one project at once. Both must start from the same shared state, and neither
// may see what the other does to it or change what the next run starts from.
func TestLiveProjectLayersAreSharedToReadAndPrivateToWrite(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to test the dedicated VM")
	}
	imageID := liveImageID(t)
	ensureLiveVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	stamp := time.Now().UTC().Format("20060102150405")
	transport := lima.NewTransport()
	projectKey := liveProject(t, transport, "livelayers")

	// Seeding stands in for publishing, which does not exist yet: the point
	// under test is what a run sees when the project already has state.
	seed := runcontainer.DefaultSpec("seed", projectKey, imageID)
	seedSnapshot(t, ctx, seed.CacheNamespacePath("npm"), liveNewerKey, "promoted", "@2000000000")
	runLive(t, ctx, "podman", "unshare", "sh", "-ec",
		"printf promoted > "+seed.ProjectPath()+"/sessions/shared.txt"+
			" && chown 1000:1000 "+seed.ProjectPath()+"/sessions/shared.txt")

	specs := map[string]runcontainer.Spec{}
	for _, name := range []string{"a", "b"} {
		selected, err := transport.SelectCacheSnapshots(
			ctx,
			projectKey,
			[]runcontainer.CacheMount{liveCache(liveNewerKey)},
		)
		if err != nil {
			t.Fatal(err)
		}
		specs[name] = liveRun(
			t, ctx, transport, projectKey, imageID,
			"livelayers-"+name+"-"+stamp, selected...,
		)
	}

	// Both runs are live throughout: each writes before either reads, so a
	// leak between them would be visible rather than merely possible.
	for name, spec := range specs {
		for _, overlay := range spec.ProjectOverlays() {
			inContainer(t, ctx, transport, spec,
				"printf %s > "+path.Join(overlay.Destination, name+".txt"), name)
		}
	}
	for name, spec := range specs {
		for _, overlay := range spec.ProjectOverlays() {
			shared := inContainer(t, ctx, transport, spec,
				"cat "+path.Join(overlay.Destination, "shared.txt"))
			if shared != "promoted" {
				t.Errorf("run %s reads %s as %q", name, overlay.Destination, shared)
			}
			listed := inContainer(t, ctx, transport, spec, "ls "+overlay.Destination)
			if want := name + ".txt shared.txt"; strings.Join(strings.Fields(listed), " ") != want {
				t.Errorf("run %s sees %s as %q, want %q", name, overlay.Destination, listed, want)
			}
		}
	}

	// A snapshot is immutable and the session store is shared, so neither may
	// carry a live run's writes into the next run of the project.
	for _, lower := range []string{
		seed.CacheNamespacePath("npm") + "/" + liveNewerKey,
		seed.ProjectPath() + "/sessions",
	} {
		if got := runLive(t, ctx, "podman", "unshare", "ls", lower); got != "shared.txt" {
			t.Errorf("project layer %s = %q after two runs wrote to it", lower, got)
		}
	}
}

// liveStoreListing is what the project's session store holds, read the way
// only pisafe can read it. Dot entries are included because a half-promoted
// transcript would be one, and finding one left behind is a failure.
func liveStoreListing(t *testing.T, ctx context.Context, store string) string {
	t.Helper()
	listed := runLive(t, ctx, "podman", "unshare", "ls", "-A", store)
	return strings.Join(strings.Fields(listed), " ")
}

// TestLiveFinishedTranscriptsPromoteWhileLiveOnesStayPrivate is what the
// session store is for: a project's next run opens with the history of its
// finished ones, and never with the transcript of a run still writing one.
// Promotion only ever adds, so a run can neither rewrite nor delete a
// transcript another run already handed over.
func TestLiveFinishedTranscriptsPromoteWhileLiveOnesStayPrivate(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to test the dedicated VM")
	}
	imageID := liveImageID(t)
	ensureLiveVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	stamp := time.Now().UTC().Format("20060102150405")
	transport := lima.NewTransport()
	projectKey := liveProject(t, transport, "livesessions")

	// A transcript's name carries a UUID, so these stand in for real ones only by
	// being distinct. The file that is not a transcript is here because the store
	// is Pi's session directory and nothing else belongs in it.
	first := liveRun(t, ctx, transport, projectKey, imageID, "livesessions-a-"+stamp)
	inContainer(t, ctx, transport, first, "printf first > /sessions/1_aaaa.jsonl")
	inContainer(t, ctx, transport, first, "printf kept > /sessions/2_bbbb.jsonl")
	inContainer(t, ctx, transport, first, "printf ignored > /sessions/notes.txt")
	runLive(t, ctx, "podman", "rm", "--force", first.ContainerName())
	if err := transport.PromoteSessions(ctx, projectKey, first.RunID); err != nil {
		t.Fatal(err)
	}

	store := first.ProjectPath() + "/sessions"
	if listed := liveStoreListing(t, ctx, store); listed != "1_aaaa.jsonl 2_bbbb.jsonl" {
		t.Fatalf("store after the first run = %q", listed)
	}

	// The whole point of the slice: a later run of the project starts with what
	// an earlier one wrote.
	second := liveRun(t, ctx, transport, projectKey, imageID, "livesessions-b-"+stamp)
	if got := inContainer(t, ctx, transport, second, "cat /sessions/1_aaaa.jsonl"); got != "first" {
		t.Errorf("the second run reads the first run's transcript as %q", got)
	}

	// The third run is live throughout, and its transcript is the one thing the
	// second run must not be able to read.
	third := liveRun(t, ctx, transport, projectKey, imageID, "livesessions-c-"+stamp)
	inContainer(t, ctx, transport, third, "printf live > /sessions/3_cccc.jsonl")
	if listed := inContainer(t, ctx, transport, second, "ls /sessions"); strings.Contains(listed, "3_cccc") {
		t.Errorf("a live transcript reached a concurrent run: %q", listed)
	}

	// Pi rewrites a transcript in place when it migrates one on load, and can
	// delete one outright from its own picker. Neither may follow the file back
	// into a store that other runs have mounted.
	inContainer(t, ctx, transport, second, "printf migrated > /sessions/1_aaaa.jsonl")
	inContainer(t, ctx, transport, second, "rm /sessions/2_bbbb.jsonl")
	inContainer(t, ctx, transport, second, "printf second > /sessions/4_dddd.jsonl")
	runLive(t, ctx, "podman", "rm", "--force", second.ContainerName())
	if err := transport.PromoteSessions(ctx, projectKey, second.RunID); err != nil {
		t.Fatal(err)
	}
	if listed := liveStoreListing(t, ctx, store); listed != "1_aaaa.jsonl 2_bbbb.jsonl 4_dddd.jsonl" {
		t.Fatalf("store after the second run = %q", listed)
	}
	for name, want := range map[string]string{
		"1_aaaa.jsonl": "first",
		"2_bbbb.jsonl": "kept",
		"4_dddd.jsonl": "second",
	} {
		got := runLive(t, ctx, "podman", "unshare", "cat", store+"/"+name)
		if got != want {
			t.Errorf("store holds %s as %q, want %q", name, got, want)
		}
	}

	// A run that starts now inherits both finished runs and neither of the live
	// one's writes, and can read what was promoted rather than merely list it.
	fourth := liveRun(t, ctx, transport, projectKey, imageID, "livesessions-d-"+stamp)
	listed := inContainer(t, ctx, transport, fourth, "ls /sessions")
	if joined := strings.Join(strings.Fields(listed), " "); joined != "1_aaaa.jsonl 2_bbbb.jsonl 4_dddd.jsonl" {
		t.Errorf("the fourth run sees %q", joined)
	}
	if got := inContainer(t, ctx, transport, fourth, "cat /sessions/4_dddd.jsonl"); got != "second" {
		t.Errorf("the fourth run reads a promoted transcript as %q", got)
	}
}

// liveRun starts one container with every mount and limit a real run gets.
// Only the SSH server it would launch is replaced, because these tests drive
// the container directly rather than over SSH.
func liveRun(
	t *testing.T,
	ctx context.Context,
	transport lima.Transport,
	projectKey string,
	imageID string,
	runID string,
	caches ...runcontainer.CacheMount,
) runcontainer.Spec {
	t.Helper()
	spec := runcontainer.DefaultSpec(runID, projectKey, imageID)
	spec.Caches = caches
	if err := transport.CreateRunStorage(ctx, runID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		runLive(t, context.Background(), "podman", "rm", "--force", "--ignore", spec.ContainerName())
		if err := transport.RemoveRunStorage(context.Background(), runID); err != nil {
			t.Errorf("remove live run storage: %v", err)
		}
	})
	if err := transport.PrepareRunLayout(ctx, runID, spec.Caches); err != nil {
		t.Fatal(err)
	}
	runArgs, err := spec.RunArgs()
	if err != nil {
		t.Fatal(err)
	}
	runArgs = append(runArgs[:len(runArgs)-2:len(runArgs)-2], "sleep", "infinity")
	if _, err := transport.Execute(ctx, nil, append([]string{"podman"}, runArgs...)...); err != nil {
		t.Fatal(err)
	}
	return spec
}

func liveImageID(t *testing.T) string {
	t.Helper()
	imageID := os.Getenv("PISAFE_LIVE_RUN_IMAGE")
	if imageID == "" {
		t.Skip("set PISAFE_LIVE_RUN_IMAGE to an immutable sha256 image ID")
	}
	return imageID
}

func inContainer(
	t *testing.T,
	ctx context.Context,
	transport lima.Transport,
	spec runcontainer.Spec,
	script string,
	arguments ...string,
) string {
	t.Helper()
	command := append(
		[]string{"podman", "exec", "--user", "1000:1000", spec.ContainerName(), "sh", "-ec", script},
		arguments...,
	)
	output, err := transport.Execute(ctx, nil, command...)
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(command, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

// TestLiveAReclaimedProjectStoreTakesEverythingWithIt is the end of a project's
// life and the one reclamation that removes something pisafe cannot rebuild. A
// checkout that has been gone for a whole window is the only thing that reaches
// here, and what it releases has to be the whole filesystem: an image left
// behind unmounted would be a project's transcripts still on disk with nothing
// left able to name them or free them.
func TestLiveAReclaimedProjectStoreTakesEverythingWithIt(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to test the dedicated VM")
	}
	imageID := liveImageID(t)
	ensureLiveVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	transport := lima.NewTransport()
	projectKey := "livereclaim-" + time.Now().UTC().Format("20060102150405")
	if err := transport.EnsureProjectStorage(ctx, projectKey); err != nil {
		t.Fatal(err)
	}
	// This test reclaims the store itself; the cleanup is for the paths that
	// fail before reaching that.
	t.Cleanup(func() {
		_ = transport.RemoveProjectStorage(context.Background(), projectKey)
	})

	store := runcontainer.DefaultSpec("reclaim", projectKey, imageID).ProjectPath()
	sessions := store + "/sessions"
	generation := store + "/cache/npm/" + liveNewerKey
	runLive(t, ctx, "podman", "unshare", "sh", "-ec",
		"printf transcript > "+sessions+"/1_aaaa.jsonl"+
			" && install -d -m 0700 -o 1000 -g 1000 "+store+"/cache/npm "+generation+
			" && printf cached > "+generation+"/shared.txt")
	if listed := liveStoreListing(t, ctx, sessions); listed != "1_aaaa.jsonl" {
		t.Fatalf("seeded session store = %q", listed)
	}
	if mounted := runLive(t, ctx, "sh", "-c",
		"mountpoint -q "+store+" && echo MOUNTED || echo UNMOUNTED",
	); mounted != "MOUNTED" {
		t.Fatalf("the project store is %s before anything reclaimed it", mounted)
	}

	if err := transport.RemoveProjectStorage(ctx, projectKey); err != nil {
		t.Fatal(err)
	}
	if mounted := runLive(t, ctx, "sh", "-c",
		"mountpoint -q "+store+" && echo MOUNTED || echo UNMOUNTED",
	); mounted != "UNMOUNTED" {
		t.Errorf("the reclaimed store is still mounted")
	}
	if left := runLive(t, ctx, "sh", "-c",
		"test -e "+store+" && echo PRESENT || echo GONE",
	); left != "GONE" {
		t.Errorf("the reclaimed store is still %s", left)
	}

	// A sweep that failed partway repeats the removal, so having nothing left
	// to do is not a failure.
	if err := transport.RemoveProjectStorage(ctx, projectKey); err != nil {
		t.Fatal(err)
	}

	// The evidence that the image went with the mount: the same key allocates a
	// filesystem with none of the old project in it.
	if err := transport.EnsureProjectStorage(ctx, projectKey); err != nil {
		t.Fatal(err)
	}
	if listed := liveStoreListing(t, ctx, sessions); listed != "" {
		t.Errorf("a reclaimed project's transcripts came back: %q", listed)
	}
	if listed := liveStoreListing(t, ctx, store+"/cache"); listed != "" {
		t.Errorf("a reclaimed project's cache came back: %q", listed)
	}
}

// TestLiveARebindCarriesTheHistoryAndNotTheCaches is what a moved or renamed
// repository gets back. A project store is keyed by a digest of the checkout
// path, so a move leaves the transcripts under a key nothing reaches any more.
// Adoption is the same additive promotion a finished run's transcripts take, so
// a name the destination already holds is left as it is rather than replaced,
// and the caches — the one part of a store that costs time and never
// correctness — are deliberately left behind.
func TestLiveARebindCarriesTheHistoryAndNotTheCaches(t *testing.T) {
	if os.Getenv("PISAFE_LIVE_LIMA") != "1" {
		t.Skip("set PISAFE_LIVE_LIMA=1 to test the dedicated VM")
	}
	ensureLiveVM(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	transport := lima.NewTransport()
	fromKey := liveProject(t, transport, "liverebindfrom")
	toKey := liveProject(t, transport, "liverebindto")
	from := runcontainer.ProjectSessionsPath(fromKey)
	to := runcontainer.ProjectSessionsPath(toKey)

	// The transcript both stores name is what Pi leaves behind when it migrates
	// one on load: the same session, rewritten. The destination's copy is the
	// one that stays.
	runLive(t, ctx, "podman", "unshare", "sh", "-ec",
		"printf superseded > "+from+"/1_aaaa.jsonl"+
			" && printf carried > "+from+"/2_bbbb.jsonl"+
			" && printf ignored > "+from+"/notes.txt"+
			" && printf kept > "+to+"/1_aaaa.jsonl"+
			" && chown 1000:1000 "+from+"/*.jsonl "+to+"/*.jsonl")
	seedSnapshot(t, ctx, path.Dir(from)+"/cache/npm", liveNewerKey, "cached", "2026-01-01")

	if err := transport.AdoptSessions(ctx, toKey, fromKey); err != nil {
		t.Fatal(err)
	}

	if listed := liveStoreListing(t, ctx, to); listed != "1_aaaa.jsonl 2_bbbb.jsonl" {
		t.Fatalf("the adopting store holds %q", listed)
	}
	for name, want := range map[string]string{"1_aaaa.jsonl": "kept", "2_bbbb.jsonl": "carried"} {
		if got := runLive(t, ctx, "podman", "unshare", "cat", to+"/"+name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if listed := liveStoreListing(t, ctx, path.Dir(to)+"/cache"); listed != "" {
		t.Errorf("a cache was carried across the rebind: %q", listed)
	}
	// The source is released separately and only once this succeeded, so until
	// then its history is still reachable under the old key.
	if listed := liveStoreListing(t, ctx, from); listed != "1_aaaa.jsonl 2_bbbb.jsonl notes.txt" {
		t.Errorf("the source store was changed by being read: %q", listed)
	}
}
