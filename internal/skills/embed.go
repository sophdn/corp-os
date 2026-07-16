package skills

import (
	"embed"
	"io/fs"
)

// The embedded skills tier ships in the binary so a fresh corpos install injects a
// profile's disciplines with no external tree present. It has TWO roots:
//
//	library/  — VANILLA: universal agent disciplines (filing, vault, planning,
//	            debugging, review-request) + stack-agnostic work disciplines. These
//	            are committed and shared in a vanilla corpos.
//	userlib/  — USER: the operator's stack-specific skills (language/framework/
//	            platform conventions like go-conventions, github-*). Its skill
//	            subdirs are GITIGNORED — embedded into THIS operator's build, absent
//	            (and clean) in a vanilla clone. Only library/README.md + userlib/
//	            README.md are committed.
//
// Both are unioned by Builtin (userlib overrides library on a name collision). The
// on-disk overlay (BuiltinWithOverride, default ~/.claude/skills) layers on top of
// both. See library/README.md and userlib/README.md for the tiering rule.
//
// Locked by tests (no hardcoded slug list): embed_test.go asserts every embedded
// skill is well-formed + Select-resolvable, and the profilehooks invariant test
// asserts every skill a builtin profile references is embedded — so a profile naming
// an un-embedded skill fails the gate (in a vanilla clone, vanilla profiles may name
// only library/ skills; user profiles + their userlib/ skills are gitignored as a set).
//
//go:embed library
var libraryFS embed.FS

//go:embed userlib
var userlibFS embed.FS

// embeddedRoots is the ordered (low→high precedence) list of embedded skill roots.
var embeddedRoots = []struct {
	fsys fs.FS
	root string
}{
	{libraryFS, "library"}, // vanilla baseline
	{userlibFS, "userlib"}, // operator overlay (gitignored content)
}

// Builtin returns a Loader backed by the embedded skills (library ∪ userlib). With
// no on-disk overlay it is fully self-contained. In a vanilla clone userlib holds no
// skills, so Builtin yields just the library/ set.
func Builtin() (*Loader, error) {
	found := map[string]Skill{}
	for _, src := range embeddedRoots {
		got, err := discoverFS(src.fsys, src.root)
		if err != nil {
			return nil, err
		}
		for n, s := range got {
			found[n] = s // later root overrides on a name collision
		}
	}
	return &Loader{builtin: sortedSkills(found)}, nil
}

// BuiltinWithOverride returns the embedded skills overlaid by an on-disk tree at
// dir: a skill present in both is taken from disk (live edits win), and the union
// is discovered. An absent/empty dir yields just the embedded baseline. This is
// the constructor the CLI uses so the active profile's disciplines are always
// available, on any machine, while a populated ~/.claude/skills still overrides.
func BuiltinWithOverride(dir string) (*Loader, error) {
	l, err := Builtin()
	if err != nil {
		return nil, err
	}
	l.dir = dir
	return l, nil
}
