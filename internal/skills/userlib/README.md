# User skills (operator-specific, gitignored)

This is the **user tier** of corpos's embedded skills. Drop a skill here
(`<slug>/SKILL.md`, frontmatter `name:` == `<slug>`) and it is embedded into *your*
corpos build and unioned with the vanilla `../library` set — but its subdirectories
are **gitignored**, so they never ship in the shared/vanilla repo. Only this README
is committed (it keeps the directory present so `//go:embed userlib` compiles in a
clean clone).

Put here the skills specific to **your** stack — language/framework conventions
(`go-conventions`, `rust-conventions`, `expo-conventions`), platform/git workflows
(`github-*`, `worktree-workflow`), debuggers, local-infra references. They are
referenced only by **user profiles** (`internal/profile/userlib/*.toml`, also
gitignored), never by vanilla profiles — so a `vanilla` corpos shared from this repo
carries none of them.

Vanilla, universal disciplines belong in `../library` (committed) instead. See
`../library/README.md` for the full core / domain / user tiering rule.
