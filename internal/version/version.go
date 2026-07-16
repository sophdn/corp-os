// Package version reports the Corp-OS build version.
package version

// version is the current Corp-OS version. Bumped manually until release
// tooling lands.
const version = "0.0.0-dev"

// Version returns the current Corp-OS version string.
func Version() string {
	return version
}
