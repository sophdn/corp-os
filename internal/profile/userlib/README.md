# User profile overlay (operator-specific, gitignored)

This is the **operator overlay** for the embedded profile library. Its `*.toml` files
are **gitignored** (only this README is committed, to keep the directory present so
`//go:embed userlib` compiles in a vanilla clone). `profile.Builtin()` merges these
onto the vanilla `../library` profiles:

- **Same name as a library profile** → the overlay entry **extends** that profile's
  `skills` (union). The entry can be partial — just `name` + `skills`. Example: a
  `git-process.toml` here with `skills = ["github-pr-workflow", "worktree-workflow"]`
  adds those to the vanilla `git-process` profile *in your build only*.
- **A new name** → added as a **new profile** (must be a complete, valid profile:
  `name`, `tier`, `[[tools]]`…). Example: a `go-coding.toml` with the coding scope +
  `skills = [..., "go-conventions"]`.

Reference your gitignored `../../skills/userlib` skills only from here — never from a
`../library` profile — so a vanilla corpos shared from this repo stays free of your
stack-specific wiring. The profilehooks invariant test still holds in both trees: in a
vanilla clone it sees only library profiles → library skills; in yours it sees the
merged set, every referenced skill embedded.
