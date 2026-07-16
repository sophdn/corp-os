package profile

import "embed"

// libraryFS embeds the §4.1 starter job-profile library so the binary ships
// self-contained (no external def dir needed on a distroless deploy). An operator
// can still override or extend it with an on-disk dir via Load.
//
//go:embed library/*.toml
var libraryFS embed.FS

// userlibFS embeds the OPERATOR profile overlay. Its *.toml files are GITIGNORED
// (only userlib/README.md is committed, which keeps the dir present so this embed
// compiles in a vanilla clone). An overlay entry whose name matches a library
// profile EXTENDS that profile's skills; a new-named entry adds a new profile. This
// is how an operator wires gitignored userlib/ skills into profiles without editing
// the committed library/ files — so a vanilla corpos shares none of it.
//
//go:embed userlib
var userlibFS embed.FS

// Builtin loads the embedded profile library (vanilla) merged with the gitignored
// userlib overlay. In a vanilla clone userlib carries no profiles, so Builtin yields
// just the library set: the mechanical leaves (task-lifecycle, file-sort, doc-filing,
// git-process, atomic-coding-chain), the judgment profiles (code-review, bug-hunt,
// design, synthesis, orchestrate), and the new bug-fix / refactor profiles.
func Builtin() (*Registry, error) {
	base, err := loadDir(libraryFS, "library", "builtin")
	if err != nil {
		return nil, err
	}
	overlay, err := loadDir(userlibFS, "userlib", "builtin-userlib")
	if err != nil {
		return nil, err
	}
	merged, err := mergeProfiles(base, overlay)
	if err != nil {
		return nil, err
	}
	return newRegistry(merged)
}
