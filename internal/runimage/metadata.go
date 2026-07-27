// Package runimage records the immutable inputs expected by the run image.
package runimage

const (
	BaseImage   = "docker.io/library/node@sha256:af01d58b748ec92b1d6e8e11429aad424fd1e68c848185399dca0596a1ab8f5c"
	PiVersion   = "0.82.0"
	PiIntegrity = "sha512-Qnqgn9zhJFQ2HZ8R4iNuGhyCk93XX6+eUw9i+TjTuo47amzCy93ft3bB6yaUCleCrNO58dJDHYSGNHv/GAPWKg=="
)

// PinnedDependency is a package Pi's shrinkwrap names without an integrity
// hash, leaving npm free to resolve its version range to a later release.
type PinnedDependency struct {
	Name      string
	Version   string
	Integrity string
}

// PinnedDependencies must move whenever PiVersion does: a shrinkwrap entry
// without an integrity hash is not a pin, so these digests are the only thing
// deciding which bytes of Pi's core, provider, and terminal packages a run gets.
var PinnedDependencies = []PinnedDependency{
	{
		Name:      "@earendil-works/pi-agent-core",
		Version:   PiVersion,
		Integrity: "sha512-bnS9DpOKK5T/F/gQkaOnYdMsuuciWiScfAHHWC+k5OQ0HxjSqMFQvp8keurULLoT4+v8NHv4V14pNvd4hsfC0Q==",
	},
	{
		Name:      "@earendil-works/pi-ai",
		Version:   PiVersion,
		Integrity: "sha512-8MvW9+zno13sXDuT2kFMnWeTNUufUhPeZDRVO+igGoBRCDWgn7Xh2FkRQI1mRuet6QhF4ENQuLYdIAOyG6BhNw==",
	},
	{
		Name:      "@earendil-works/pi-tui",
		Version:   PiVersion,
		Integrity: "sha512-9IDjQOXne7t9l2s2YcjnIBxsVNVPE7qScVSB3YmFlXsBW4pfo2gOElTxggV84KrRiGqABnlFPBWbf0k54hszHQ==",
	},
}
