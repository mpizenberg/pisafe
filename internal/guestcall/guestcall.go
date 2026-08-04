// Package guestcall names every call the controller makes into the helper
// inside a run image. The controller and the helper are separate binaries built
// from the same source tree, and nothing at run time can repair a disagreement
// between them: the helper answers a call it does not know with a usage error,
// halfway through creating a run.
package guestcall

const (
	Materialize       = "materialize"
	PrepareApply      = "prepare-apply"
	Diff              = "diff"
	Export            = "export"
	Import            = "import"
	ConfigureSSH      = "configure-ssh"
	ConfigureModels   = "configure-models"
	ConfigureIdentity = "configure-identity"
	ConfigureProfile  = "configure-profile"
	ServeSSH          = "serve-ssh"
	ProxySSH          = "proxy-ssh"
)

// Contract is every call with the arguments it takes. The helper prints it when
// a call does not parse, which is what puts these bytes in the built binary; the
// controller refuses a packaged helper that does not carry the exact text it was
// itself built with. A call whose meaning changes without its name changing is
// invisible to that check, so such a change renames the call.
const Contract = "pisafe-guest <" +
	Materialize + " <stage-directory> <workspace>" +
	"|" + PrepareApply + " <keep|drop> <workspace> <package-directory>" +
	"|" + Diff + " <workspace>" +
	"|" + Export + " <workspace> <path>" +
	"|" + Import + " <workspace> <destination> <name> <replace|refuse>" +
	"|" + ConfigureSSH +
	"|" + ConfigureModels +
	"|" + ConfigureIdentity +
	"|" + ConfigureProfile +
	"|" + ServeSSH +
	"|" + ProxySSH + ">"
