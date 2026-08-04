// Package runcontainer defines the rootless Podman resources and hardened
// command line for one isolated run.
package runcontainer

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/guestcall"
	"github.com/mpizenberg/pisafe/internal/profile"
	"github.com/mpizenberg/pisafe/internal/runcopy"
	"github.com/mpizenberg/pisafe/internal/runid"
)

const (
	DefaultCPUs       = "2"
	DefaultMemory     = int64(4 * 1024 * 1024 * 1024)
	DefaultPIDs       = 512
	DefaultTemporary  = int64(1024 * 1024 * 1024)
	DefaultPersistent = int64(10 * 1024 * 1024 * 1024)
	DefaultProject    = int64(10 * 1024 * 1024 * 1024)
	// DefaultGlobal bounds the profile. It holds installed extensions and their
	// dependencies for every project at once, which is a fraction of what one
	// project's dependency cache is, and nothing writes it except a command the
	// user runs deliberately.
	DefaultGlobal    = int64(2 * 1024 * 1024 * 1024)
	DefaultWallClock = int64(8 * 60 * 60)
	// packageSeconds bounds an install. It is generous for fetching one
	// package and its dependencies, and short enough that a wedged registry
	// fails the command rather than hanging it.
	packageSeconds = 600
	// DefaultCacheGenerations bounds how many published generations one cache
	// namespace keeps, beyond any a run may still mount. One is enough for the
	// fallback to work — an unmatched key restores the newest generation, and
	// there is always exactly one — while keeping a namespace's cost to a
	// single copy of the cache on a fixed-capacity image.
	DefaultCacheGenerations = 1
	// applyPackage is where a run leaves the bundles apply fetches. It sits in
	// the run's own workspace, the only writable place both the run and the
	// VM-side transport can reach.
	applyPackage      = "apply"
	containerUser     = "1000:1000"
	containerWorkRoot = "/work"
	containerHome     = "/home/node"
	// imagePath is the run image's own search path. pisafe restates it because
	// naming PATH at all replaces what the image set, and dropping any of it
	// would take node and git with it.
	imagePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	// containerCacheRoot is a layout pisafe owns rather than a tool's own
	// default location. Nothing is shared unless a cache-specific environment
	// variable points it here, so the layer cannot accumulate arbitrary state.
	containerCacheRoot   = "/cache"
	containerSessionRoot = "/sessions"
	// containerProfileRoot is where the profile's packages are mounted. It is
	// deliberately not Pi's own global package store: mounting there read-only
	// made pi install fail, and an agent told it cannot install globally
	// installs into the repository instead, where the package is committed and
	// arrives on the user's Mac. Pi's store stays an ordinary directory in the
	// run's home, so a global install succeeds, serves that run, and dies with
	// it, while nothing a run does reaches the profile at all.
	containerProfileRoot = "/opt/pisafe/profile"
	// containerToolRoot is where the installed commands are mounted. It is
	// outside the run's home, which is writable, and outside the image's own
	// directories, which the run must not be able to appear to have changed.
	containerToolRoot = "/opt/pisafe/tools"
	guestStorageRoot  = "/var/lib/pisafe/runs"
	guestProjectRoot  = "/var/lib/pisafe/projects"
	guestGlobalRoot   = "/var/lib/pisafe/global"

	// cacheNamespace holds one directory of immutable snapshots per declared
	// cache. sessionsNamespace is the one shared directory that is not a cache:
	// it has no key and no invalidation, because a transcript's name is a
	// session ID and nothing is implied by its presence.
	cacheNamespace    = "cache"
	sessionsNamespace = "sessions"

	// ProfileName is the one profile every run mounts. The path keeps a name
	// level so a second profile can exist later; nothing reads one from a run.
	ProfileName = "default"

	// extensionsNamespace holds one npm prefix root per installed extension,
	// toolsNamespace the same per installed command, and pinsNamespace pisafe's
	// record of what each was pinned to. Only the first two are mounted: what a
	// run needs is the packages, not the bookkeeping.
	extensionsNamespace = "extensions"
	toolsNamespace      = "tools"
	pinsNamespace       = "pins"

	// toolBinDirectory is the single entry the installed commands add to a
	// run's PATH. It holds a relative link per claimed name, so a tool is
	// reachable without its module root having to be searched for anything else.
	toolBinDirectory = "bin"
)

// ProjectNamespaces is for the privileged helper, which allocates these two
// directories in a project filesystem and nothing below them. What lives below
// depends on the repository, which the helper must never learn about.
func ProjectNamespaces() []string {
	return []string{cacheNamespace, sessionsNamespace}
}

// GlobalNamespaces is the same for the profile filesystem. What lives below
// depends on what the user installed, which the helper never learns either.
func GlobalNamespaces() []string {
	return []string{extensionsNamespace, toolsNamespace, pinsNamespace}
}

func ProfilePath() string {
	return guestGlobalRoot + "/" + ProfileName
}

// ProfilePinsPath holds pisafe's record of every installed extension. It is
// deliberately outside what a run mounts.
func ProfilePinsPath() string {
	return ProfilePath() + "/" + pinsNamespace
}

// Mount is one path a run receives whole, as opposed to an overlay it stacks
// its own writes on.
type Mount struct {
	Destination string
	Source      string
}

// ProfileMount is the shared read-only profile. Every run gets the same one,
// so it depends on nothing about the run.
func ProfileMount() Mount {
	return Mount{
		Destination: containerProfileRoot,
		Source:      ProfilePath() + "/" + extensionsNamespace,
	}
}

// ToolsMount is the shared read-only directory of installed commands. Like the
// profile packages it depends on nothing about the run, and like them a run has
// no writable handle on it at all.
func ToolsMount() Mount {
	return Mount{
		Destination: containerToolRoot,
		Source:      ProfilePath() + "/" + toolsNamespace,
	}
}

// ToolBinPath is the directory of links a run searches, on the VM side.
func ToolBinPath() string {
	return ToolsMount().Source + "/" + toolBinDirectory
}

// ExtensionInstallRoot is the module root one extension is installed into.
// Each gets its own, so installing a second never merges dependency trees with
// the first.
func ExtensionInstallRoot(directory string) string {
	return ProfileMount().Source + "/" + directory
}

// ToolInstallRoot is the same for one installed command.
func ToolInstallRoot(directory string) string {
	return ToolsMount().Source + "/" + directory
}

// ToolBinaryTarget is what one link in the shared directory points at. It is
// relative, so the whole namespace resolves identically on the VM and at the
// path a run mounts it on.
func ToolBinaryTarget(directory, binary string) string {
	return "../" + directory + "/node_modules/.bin/" + binary
}

var (
	imageIDPattern         = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	cacheKeyPattern        = regexp.MustCompile(`^[a-f0-9]{16}$`)
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// CacheMount is one cache the repository declared, resolved for one run. Key
// is what this run's inputs hash to; Snapshot is what it actually starts from,
// which is Key when the project has published that generation, an older
// generation when it has not, and nothing when the namespace is empty.
type CacheMount struct {
	Name     string   `json:"name"`
	Env      []string `json:"env"`
	Key      string   `json:"key"`
	Snapshot string   `json:"snapshot,omitempty"`
}

func (cache CacheMount) Validate() error {
	if err := runid.Validate(cache.Name); err != nil {
		return fmt.Errorf("invalid cache name %q", cache.Name)
	}
	if err := ValidateCacheGeneration(cache.Key); err != nil {
		return fmt.Errorf("cache %q has an invalid key: %w", cache.Name, err)
	}
	if cache.Snapshot != "" {
		if err := ValidateCacheGeneration(cache.Snapshot); err != nil {
			return fmt.Errorf("cache %q has an invalid snapshot: %w", cache.Name, err)
		}
	}
	return ValidateCacheEnvironment(cache.Env)
}

// ValidateCacheGeneration bounds what may name a directory of published cache
// state. A generation name reaches pisafe from a directory listing as well as
// from its own keying, so it is checked wherever it becomes half of a path.
func ValidateCacheGeneration(name string) error {
	if !cacheKeyPattern.MatchString(name) {
		return fmt.Errorf("invalid cache generation %q", name)
	}
	return nil
}

// ValidateCacheEnvironment refuses a variable pisafe sets itself. Rebinding
// one of them would move the session directory, the home, or npm's log path
// into state the project shares between runs. Every other name is permitted:
// what a cache carries into a later run of the same repository is the
// repository's own business.
func ValidateCacheEnvironment(names []string) error {
	for _, name := range names {
		if !environmentNamePattern.MatchString(name) {
			return fmt.Errorf("invalid environment variable name %q", name)
		}
		if slices.ContainsFunc(runEnvironment, func(variable [2]string) bool {
			return variable[0] == name
		}) {
			return fmt.Errorf("a cache may not rebind %s", name)
		}
	}
	return nil
}

type Spec struct {
	RunID       string
	ProjectKey  string
	ImageID     string
	Caches      []CacheMount
	CPUs        string
	MemoryBytes int64
	PIDs        int
	TmpBytes    int64
	WallSeconds int64
}

func DefaultSpec(runID, projectKey, imageID string) Spec {
	return Spec{
		RunID:       runID,
		ProjectKey:  projectKey,
		ImageID:     imageID,
		CPUs:        DefaultCPUs,
		MemoryBytes: DefaultMemory,
		PIDs:        DefaultPIDs,
		TmpBytes:    DefaultTemporary,
		WallSeconds: DefaultWallClock,
	}
}

func (spec Spec) Validate() error {
	if err := runid.Validate(spec.RunID); err != nil {
		return err
	}
	if err := runid.Validate(spec.ProjectKey); err != nil {
		return fmt.Errorf("invalid project key: %w", err)
	}
	if !imageIDPattern.MatchString(spec.ImageID) {
		return fmt.Errorf("image must be an immutable sha256 ID")
	}
	seen := make(map[string]bool, len(spec.Caches))
	for _, cache := range spec.Caches {
		if err := cache.Validate(); err != nil {
			return err
		}
		if seen[cache.Name] {
			return fmt.Errorf("cache %q is declared twice", cache.Name)
		}
		seen[cache.Name] = true
	}
	if spec.CPUs == "" {
		return fmt.Errorf("CPU limit is required")
	}
	if spec.MemoryBytes <= 0 {
		return fmt.Errorf("memory limit is required")
	}
	if spec.PIDs <= 0 {
		return fmt.Errorf("process limit is required")
	}
	if spec.TmpBytes <= 0 {
		return fmt.Errorf("temporary-filesystem limit is required")
	}
	if spec.WallSeconds <= 0 {
		return fmt.Errorf("wall-clock limit is required")
	}
	return nil
}

func (spec Spec) ContainerName() string {
	return "pisafe-run-" + spec.RunID
}

func (spec Spec) StoragePath() string {
	return guestStorageRoot + "/" + spec.RunID
}

func (spec Spec) WorkspacePath() string {
	return spec.StoragePath() + "/workspace"
}

func (spec Spec) HomePath() string {
	return spec.StoragePath() + "/home"
}

func (spec Spec) ProjectPath() string {
	return guestProjectRoot + "/" + spec.ProjectKey
}

// ProjectSessionsPath is one project's session store, and RunSessionUpperPath
// is where one run's own transcripts land until they are promoted into it.
// Both are named without a Spec because transcripts move between projects and
// out of finished runs, neither of which is something a run is configured for.
func ProjectSessionsPath(projectKey string) string {
	return guestProjectRoot + "/" + projectKey + "/" + sessionsNamespace
}

func RunSessionUpperPath(runID string) string {
	return guestStorageRoot + "/" + runID + "/overlay/" + sessionsNamespace + "/upper"
}

// CacheNamespacePath is where every published generation of one declared cache
// lives. The directory is the restore prefix: falling back means taking the
// newest entry in it.
func (spec Spec) CacheNamespacePath(name string) string {
	return spec.ProjectPath() + "/" + cacheNamespace + "/" + name
}

// overlayRoot is the run-private half of one overlay. pisafe creates these
// itself, because the set of cache names comes from the repository and so is
// unknown when the run filesystem is allocated.
func (spec Spec) overlayRoot(namespace string) string {
	return spec.StoragePath() + "/overlay/" + namespace
}

// ProjectOverlay is one shared layer as the run sees it. Nothing already in
// Lower is rewritten while a run holds it: a cache generation is immutable, and
// the session store only ever gains transcripts. Upper and Work belong to the
// run alone, which is what keeps one run's writes out of every other run.
type ProjectOverlay struct {
	Destination string
	Lower       string
	Upper       string
	Work        string
}

func (spec Spec) ProjectOverlays() []ProjectOverlay {
	overlays := []ProjectOverlay{spec.sessionsOverlay()}
	for _, cache := range spec.Caches {
		overlays = append(overlays, spec.cacheOverlay(cache))
	}
	return overlays
}

func (spec Spec) sessionsOverlay() ProjectOverlay {
	return ProjectOverlay{
		Destination: containerSessionRoot,
		Lower:       ProjectSessionsPath(spec.ProjectKey),
		Upper:       RunSessionUpperPath(spec.RunID),
		Work:        spec.overlayRoot(sessionsNamespace) + "/work",
	}
}

func (spec Spec) cacheOverlay(cache CacheMount) ProjectOverlay {
	private := spec.overlayRoot(cacheNamespace + "/" + cache.Name)
	lower := private + "/lower"
	if cache.Snapshot != "" {
		lower = spec.CacheNamespacePath(cache.Name) + "/" + cache.Snapshot
	}
	return ProjectOverlay{
		Destination: containerCacheRoot + "/" + cache.Name,
		Lower:       lower,
		Upper:       private + "/upper",
		Work:        private + "/work",
	}
}

// runEnvironment is what pisafe sets in every run, and so is exactly what a
// repository may not rebind through a declared cache.
var runEnvironment = [][2]string{
	{"HOME", containerHome},
	// The image's own binaries come first, so an installed tool never decides
	// what git or node means. What a run puts in its home directory is reachable
	// but last, which is where uv and pip leave an executable.
	{"PATH", imagePath + ":" + containerToolRoot + "/" + toolBinDirectory +
		":" + containerHome + "/.local/bin"},
	{"GIT_TERMINAL_PROMPT", "0"},
	{"PI_CODING_AGENT_DIR", containerHome + "/.pi/agent"},
	{"PI_CODING_AGENT_SESSION_DIR", containerSessionRoot},
	{"PI_SKIP_VERSION_CHECK", "1"},
	// npm defaults its logs and update stamp into the cache directory, so a
	// project sharing an npm cache would publish per-run state that grows
	// without bound and is useful to no later run.
	{"npm_config_logs_dir", containerHome + "/.npm/_logs"},
	{"npm_config_update_notifier", "false"},
}

// volume renders the only spelling Podman accepts for a rootless overlay.
// It refuses nodev and nosuid alongside one, which costs nothing here:
// creating a device needs CAP_MKNOD and the run drops every capability, and
// no-new-privileges already neutralizes a setuid bit.
func (overlay ProjectOverlay) volume() string {
	return overlay.Lower + ":" + overlay.Destination +
		":O,upperdir=" + overlay.Upper + ",workdir=" + overlay.Work
}

func (spec Spec) RunArgs() ([]string, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	memory := strconv.FormatInt(spec.MemoryBytes, 10)
	tmpSize := strconv.FormatInt(spec.TmpBytes, 10)
	args := []string{
		"run",
		"--detach",
		"--pull=never",
		"--name", spec.ContainerName(),
		"--hostname", spec.ContainerName(),
		"--label", "io.pisafe.run=" + spec.RunID,
		"--user", containerUser,
		"--read-only",
		"--cap-drop=all",
		"--security-opt=no-new-privileges",
		"--network=pasta",
		"--dns=1.1.1.1",
		"--dns=9.9.9.9",
		"--cpus", spec.CPUs,
		"--memory", memory,
		"--memory-swap", memory,
		"--pids-limit", strconv.Itoa(spec.PIDs),
		"--timeout", strconv.FormatInt(spec.WallSeconds, 10),
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=" + tmpSize,
		"--mount", "type=tmpfs,dst=/run,tmpfs-size=16777216,tmpfs-mode=0755,U=true",
		"--mount", "type=bind,src=" + spec.WorkspacePath() + ",dst=" + containerWorkRoot + ",nodev,nosuid",
		"--mount", "type=bind,src=" + spec.HomePath() + ",dst=" + containerHome + ",nodev,nosuid",
		"--mount", "type=bind,src=" + ProfileMount().Source +
			",dst=" + ProfileMount().Destination + ",ro,nodev,nosuid",
		"--mount", "type=bind,src=" + ToolsMount().Source +
			",dst=" + ToolsMount().Destination + ",ro,nodev,nosuid",
	}
	for _, overlay := range spec.ProjectOverlays() {
		args = append(args, "--volume", overlay.volume())
	}
	args = append(args, "--workdir", containerWorkRoot)
	for _, variable := range runEnvironment {
		args = append(args, "--env", variable[0]+"="+variable[1])
	}
	for _, cache := range spec.Caches {
		for _, name := range cache.Env {
			args = append(args, "--env", name+"="+containerCacheRoot+"/"+cache.Name)
		}
	}
	return append(args,
		spec.ImageID,
		"pisafe-guest", guestcall.ServeSSH,
	), nil
}

// PublishArgs renders the throwaway container that reads one cache's merged
// view out as a tar stream. Only overlayfs knows what a run's writes and
// deletions add up to, so the state worth keeping is taken from a container
// that mounts the overlay rather than reconstructed from the upper. The
// container gets no network, no home, and no workspace, because copying a
// directory needs none of them.
func (spec Spec) PublishArgs(cache CacheMount) ([]string, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := cache.Validate(); err != nil {
		return nil, err
	}
	overlay := spec.cacheOverlay(cache)
	memory := strconv.FormatInt(spec.MemoryBytes, 10)
	return []string{
		"run",
		"--rm",
		"--pull=never",
		"--label", "io.pisafe.run=" + spec.RunID,
		"--label", "io.pisafe.kind=publish",
		"--user", containerUser,
		"--read-only",
		"--cap-drop=all",
		"--security-opt=no-new-privileges",
		"--network=none",
		"--memory", memory,
		"--memory-swap", memory,
		"--pids-limit", strconv.Itoa(spec.PIDs),
		"--volume", overlay.volume(),
		spec.ImageID,
		"tar", "--numeric-owner", "--owner=1000", "--group=1000",
		"-C", overlay.Destination, "-cf", "-", ".",
	}, nil
}

// PackageResolveArgs renders the throwaway container that asks npm what a
// spec resolves to. It reports npm's own JSON, which carries the exact version
// and the integrity of the tarball that spec names, so the pin comes from the
// registry's own answer rather than from anything pisafe invents.
func PackageResolveArgs(imageID, packageSpec string) ([]string, error) {
	args, err := packageArgs(imageID, "resolve")
	if err != nil {
		return nil, err
	}
	return append(args, "bash", "-ceu", resolvePackageScript, "pisafe-resolve", packageSpec), nil
}

// PackageInstallArgs renders the throwaway container that fetches one exact
// release, checks it against the pin pisafe already holds, installs it into a
// module root of its own, and streams the tree out. It writes nothing outside
// its own temporary filesystem: no container ever holds the profile open for
// writing, exactly as no container holds the project store open.
func PackageInstallArgs(imageID, name, version, integrity string) ([]string, error) {
	if err := profile.ValidatePackageName(name); err != nil {
		return nil, err
	}
	if err := profile.ValidateVersion(version); err != nil {
		return nil, err
	}
	if err := profile.ValidateIntegrity(integrity); err != nil {
		return nil, err
	}
	args, err := packageArgs(imageID, "install")
	if err != nil {
		return nil, err
	}
	return append(
		args,
		"bash", "-ceu", installPackageScript, "pisafe-install", name, version, integrity,
	), nil
}

// packageArgs is the container both halves of an install run in. It is a run
// container in every respect except that it has no storage at all: managing the
// profile is the one thing pisafe does that needs the network and needs nothing
// of any run or project.
func packageArgs(imageID, kind string) ([]string, error) {
	if !imageIDPattern.MatchString(imageID) {
		return nil, fmt.Errorf("image must be an immutable sha256 ID")
	}
	memory := strconv.FormatInt(DefaultMemory, 10)
	return []string{
		"run",
		"--rm",
		"--pull=never",
		"--label", "io.pisafe.kind=package-" + kind,
		"--user", containerUser,
		"--read-only",
		"--cap-drop=all",
		"--security-opt=no-new-privileges",
		"--network=pasta",
		"--dns=1.1.1.1",
		"--dns=9.9.9.9",
		"--memory", memory,
		"--memory-swap", memory,
		"--pids-limit", strconv.Itoa(DefaultPIDs),
		"--timeout", strconv.Itoa(packageSeconds),
		"--tmpfs", "/tmp:rw,nosuid,nodev,size=" + strconv.FormatInt(DefaultTemporary, 10),
		"--env", "HOME=/tmp",
		"--env", "npm_config_cache=/tmp/npm-cache",
		"--env", "npm_config_update_notifier=false",
		"--env", "npm_config_fund=false",
		"--env", "npm_config_audit=false",
		"--workdir", "/tmp",
		imageID,
	}, nil
}

const (
	resolvePackageScript = `exec npm pack --dry-run --json -- "$1"`

	// installPackageScript fetches the pinned release, refuses to install
	// anything whose bytes do not hash to what pisafe recorded, and then lets
	// npm resolve the package's own dependencies into the same root. Install
	// scripts are not run: a package that needs a build step is not installable
	// this way, which is the same discipline the run image is built with.
	installPackageScript = `
tarball=$(npm pack --ignore-scripts --silent -- "$1@$2")
test "sha512-$(openssl dgst -sha512 -binary "$tarball" | openssl base64 -A)" = "$3"
mkdir /tmp/root
npm install --prefix /tmp/root --ignore-scripts --legacy-peer-deps "./$tarball" >&2
rm -f "$tarball"
test -d "/tmp/root/node_modules/$1"
exec tar --numeric-owner --owner=1000 --group=1000 -C /tmp/root -cf - .
`
)

func (spec Spec) ConfigureSSHArgs() ([]string, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return []string{
		"run",
		"--rm",
		"--interactive",
		"--pull=never",
		"--label", "io.pisafe.run=" + spec.RunID,
		"--label", "io.pisafe.kind=ssh-init",
		"--user", containerUser,
		"--read-only",
		"--cap-drop=all",
		"--security-opt=no-new-privileges",
		"--network=none",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=16777216",
		"--mount", "type=bind,src=" + spec.HomePath() + ",dst=" + containerHome + ",nodev,nosuid",
		"--env", "HOME=" + containerHome,
		spec.ImageID,
		"pisafe-guest", guestcall.ConfigureSSH,
	}, nil
}

func (spec Spec) MaterializeArgs(projectDirectory string) ([]string, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := runid.Validate(projectDirectory); err != nil {
		return nil, fmt.Errorf("invalid project directory: %w", err)
	}
	return []string{
		"exec",
		"--user", containerUser,
		"--workdir", containerWorkRoot,
		spec.ContainerName(),
		"pisafe-guest", guestcall.Materialize,
		containerWorkRoot + "/stage",
		containerWorkRoot + "/" + projectDirectory,
	}, nil
}

// ConfigureModelsArgs installs the models a run may reach and the one it opens
// on, piped through stdin into the run home. It runs after activation and
// resume so a fresh capability always replaces the previous one.
func (spec Spec) ConfigureModelsArgs() ([]string, error) {
	return spec.configureArgs(guestcall.ConfigureModels)
}

// ConfigureIdentityArgs installs the Git identity piped through stdin into the
// run home. It runs once at creation, because the run home keeps it.
func (spec Spec) ConfigureIdentityArgs() ([]string, error) {
	return spec.configureArgs(guestcall.ConfigureIdentity)
}

// ConfigureProfileArgs tells the run which profile packages to load and which
// directory it may load project resources from. It runs after activation and
// resume, because the profile changes between runs and the run's own copy of
// what it says does not survive one.
func (spec Spec) ConfigureProfileArgs() ([]string, error) {
	return spec.configureArgs(guestcall.ConfigureProfile)
}

func (spec Spec) configureArgs(command string) ([]string, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return []string{
		"exec",
		"--interactive",
		"--user", containerUser,
		spec.ContainerName(),
		"pisafe-guest", command,
	}, nil
}

// PrepareApplyArgs captures a run's result in a throwaway container over the
// run's persistent workspace.
func (spec Spec) PrepareApplyArgs(
	projectDirectory string,
	baseline gitstage.BaselineChoice,
) ([]string, error) {
	if _, err := gitstage.ParseBaselineChoice(string(baseline)); err != nil {
		return nil, err
	}
	args, err := spec.inspectionArgs("apply", projectDirectory, "")
	if err != nil {
		return nil, err
	}
	return append(
		args,
		"pisafe-guest", guestcall.PrepareApply,
		string(baseline),
		containerWorkRoot+"/"+projectDirectory,
		containerWorkRoot+"/"+applyPackage,
	), nil
}

// DiffArgs reports what a run changed. The workspace is mounted read-only
// because a report must not be able to alter what it reports, which also lets
// it run against a container that is still working.
func (spec Spec) DiffArgs(projectDirectory string) ([]string, error) {
	args, err := spec.inspectionArgs("diff", projectDirectory, ",ro")
	if err != nil {
		return nil, err
	}
	return append(args, "pisafe-guest", guestcall.Diff, containerWorkRoot+"/"+projectDirectory), nil
}

// ExportArgs streams one path of the run's workspace out as a tar on standard
// output. The workspace is read-only for the same reason diff's is: taking a
// copy must not change what was copied.
func (spec Spec) ExportArgs(projectDirectory, requestPath string) ([]string, error) {
	requested, err := runcopy.SafePath(requestPath)
	if err != nil {
		return nil, err
	}
	args, err := spec.inspectionArgs("copy", projectDirectory, ",ro")
	if err != nil {
		return nil, err
	}
	return append(
		args,
		"pisafe-guest", guestcall.Export,
		containerWorkRoot+"/"+projectDirectory,
		requested,
	), nil
}

// ImportArgs unpacks an archive read from standard input into the run's
// workspace. It is the one inspection whose workspace mount is writable, since
// putting a file into a run is the whole of what it does; everything else the
// throwaway container is denied it stays denied, including the network.
func (spec Spec) ImportArgs(
	projectDirectory string,
	destination string,
	name string,
	replace bool,
) ([]string, error) {
	if destination != "" {
		cleaned, err := runcopy.SafePath(destination)
		if err != nil {
			return nil, err
		}
		destination = cleaned
	}
	if name == "" || name != path.Base(name) || name == "." || name == ".." {
		return nil, fmt.Errorf("%q is not a name a copy can arrive under", name)
	}
	args, err := spec.inspectionArgs("copy-in", projectDirectory, "")
	if err != nil {
		return nil, err
	}
	decision := "refuse"
	if replace {
		decision = "replace"
	}
	return append(
		args,
		"pisafe-guest", guestcall.Import,
		containerWorkRoot+"/"+projectDirectory,
		destination,
		name,
		decision,
	), nil
}

// inspectionArgs builds a throwaway container over the run's persistent
// workspace, up to and including the image. Inspecting a run needs no network,
// no home, and no part of its wall-clock budget, so it gets none of them, and
// it works whether or not the run container exists.
func (spec Spec) inspectionArgs(
	kind string,
	projectDirectory string,
	mountOptions string,
) ([]string, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := runid.Validate(projectDirectory); err != nil {
		return nil, fmt.Errorf("invalid project directory: %w", err)
	}
	memory := strconv.FormatInt(spec.MemoryBytes, 10)
	return []string{
		"run",
		"--rm",
		"--interactive",
		"--pull=never",
		"--label", "io.pisafe.run=" + spec.RunID,
		"--label", "io.pisafe.kind=" + kind,
		"--user", containerUser,
		"--read-only",
		"--cap-drop=all",
		"--security-opt=no-new-privileges",
		"--network=none",
		"--memory", memory,
		"--memory-swap", memory,
		"--pids-limit", strconv.Itoa(spec.PIDs),
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=" + strconv.FormatInt(spec.TmpBytes, 10),
		"--mount", "type=bind,src=" + spec.WorkspacePath() +
			",dst=" + containerWorkRoot + ",nodev,nosuid" + mountOptions,
		"--workdir", containerWorkRoot,
		"--env", "HOME=/tmp",
		"--env", "GIT_TERMINAL_PROMPT=0",
		spec.ImageID,
	}, nil
}

func (spec Spec) CleanupStageArgs() ([]string, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return []string{
		"exec",
		"--user", containerUser,
		spec.ContainerName(),
		"rm", "-rf", containerWorkRoot + "/stage",
	}, nil
}
