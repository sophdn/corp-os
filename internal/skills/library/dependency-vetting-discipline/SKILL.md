---
name: dependency-vetting-discipline
description: "Before adding ANY third-party dependency in ANY ecosystem (Go module, npm, PyPI, cargo, system package, build/CI tool), vet it across three complementary layers — provenance/maintenance, automated vuln scan (ecosystem DB + OSV breadth), and a MANUAL web scan for fresh supply-chain incidents the curated DBs lag on — then pin it to an exact version. Codifies the per-ecosystem tooling table, the non-redundant three-layer model (why a green scanner is not sufficient), the transitive-dep + provenance checks, and the pin-not-float rule. Cross-project: any repo, any language."
triggers:
  - add dependency
  - add a dependency
  - new dependency
  - install package
  - install a package
  - add a library
  - pull in a package
  - pull in a library
  - third-party package
  - third party dependency
  - go get
  - go get -tool
  - go install
  - npm install
  - npm add
  - yarn add
  - pnpm add
  - pip install
  - poetry add
  - uv add
  - cargo add
  - vet dependency
  - vet the dependency
  - vet this package
  - is this package safe
  - is this dependency safe
  - supply chain
  - supply-chain
  - dependency audit
  - audit dependencies
  - pin the version
  - pin versions
  - version pinning
  - govulncheck
  - osv-scanner
  - cargo audit
  - pip-audit
  - npm audit
---

# Dependency Vetting Discipline

## Core

Vet EVERY new third-party dependency (any ecosystem — Go / npm / PyPI / cargo / system / build-tool) across three complementary layers, then pin — BEFORE `go get` / `npm install` / `pip install` / `cargo add`, not after the import compiles:

1. **Provenance + maintenance.** Real maintainers, recent commits, sane issue tracker, download/star sanity, license fit. A typo-squat or abandoned package fails here.
2. **Automated vuln scan.** The ecosystem DB AND OSV breadth (`govulncheck` / `npm audit` / `pip-audit` / `cargo audit`, plus `osv-scanner`). Scan transitive deps too.
3. **Manual web scan.** Search for fresh supply-chain incidents the curated DBs lag on (package name + "compromise" / "malware" / "CVE"). A green scanner is NOT sufficient — layer 3 catches what layers 1–2 haven't ingested yet.

Then **pin to an exact version** (no `^` / `~` / floating ranges; commit the lockfile). Build/CI/dev tools (linters, codegen, mutation testers) are dependencies too — they run with your privileges, so vet them the same way. If any layer is unclear, STOP and surface it — don't import on hope.

## The one-line definition

**Every new third-party dependency is vetted across three complementary layers and then pinned to an exact version before it enters the build.** Vetting is a *gate*, not a follow-up: you do it before `go get` / `npm install` / `pip install` / `cargo add`, not after the import already compiles. Cross-ecosystem — the layers are the same; only the tools change.

## When this skill fires

- A phrase proposing a new dependency: "add X", "let's use the Y library", "go get …", "npm install …", "pip install …", "cargo add …", "pull in …".
- Installing a build/CI/dev tool (linters, codegen, mutation testers) — these are dependencies too, and they run with your privileges.
- An audit-shaped question about existing dependencies: "are our deps safe", "any vulnerable packages", "pin the versions".
- The reflex phrases: "supply chain", "is this package safe", "vet this".

## The three-layer vet (the gate)

No single layer is sufficient — they cover *different* failure modes. Run all three before admitting a dependency.

### Layer 1 — Provenance & maintenance
- Who publishes it? Real org / known maintainer / official project, vs a personal repo with one commit. Prefer first-party (e.g. `google/jsonschema-go`) and packages with **few or zero transitive deps** (smaller attack surface).
- **Is it maintained?** Check last release, open-issue responsiveness, and — critically — whether the repo is **archived / deprecated**. An archived package gets *no future security fixes*; that is a supply-chain risk even with a clean current scan. (Worked example below: `gopkg.in/yaml.v3` was archived 2026-04-01; the scanners were green, but the right move was migrating to the maintained fork.)
- Typosquat check: confirm the import path is the real one, character-for-character (`mattn/go-sqlite3`, not `mattn/go-sqlite`).

### Layer 2 — Automated vuln scan (ecosystem DB + OSV breadth)
Run BOTH an ecosystem-native reachability scanner AND a broad OSV scanner:

| Ecosystem | Reachability / native | Breadth (OSV + malicious-packages) |
|---|---|---|
| Go | `govulncheck ./...` (reachability-filtered) — **authoritative for Go** | manual web scan (see Layer 3); **NOT osv-scanner** for Go — see caveat |
| npm / JS | `npm audit` (or `pnpm audit`) | `osv-scanner` + Socket.dev for behavior signals |
| Python | `pip-audit` (or `safety`) | `osv-scanner scan source --lockfile=…` |
| Rust | `cargo audit` (RustSec) | `osv-scanner` |

- `govulncheck`-style reachability tells you whether *your code path actually reaches* the vuln — a finding you don't call is lower priority than one you do, but still record it and bump it (a present-but-unreachable vuln is one refactor away from reachable).
- `osv-scanner` casts wider (GHSA across ecosystems + the OSV malicious-packages dataset) for **npm/PyPI/cargo**. Run it on the lockfile.
- **Go caveat (osv-scanner false-cleans):** osv-scanner v2.3.8 was found to report 0 vulns for known-vulnerable Go deps even with the advisory in its local DB and the version correctly extracted (bug `osv-scanner-v2-silently-false-cleans-go-advisories`). **Do not trust osv-scanner for the Go ecosystem.** Root cause is upstream, not a flag we're missing: osv-scanner's Go path doesn't match advisories directly — it derives a custom database from `go.mod` and shells out to a govulncheck-style matcher, and that path has a documented class of false-negatives (it never sees stdlib vulns because stdlib isn't in `go.mod` — [google/osv-scanner#453](https://github.com/google/osv-scanner/issues/453) — and mis-matches regular-module version ranges as we observed for `golang.org/x/text@0.3.0`). For Go, run `govulncheck` directly (Go-team-maintained, reachability-aware, the one that catches real findings) as the matcher; breadth comes from the manual web scan. Re-evaluate if a fixed osv-scanner version lands.

### Layer 3 — Manual web scan (the non-redundant layer)
**Layers 1–2 only know what's already cataloged.** They are blind to: a fresh supply-chain injection published hours ago, a maintainer-account compromise before triage, a typosquat not yet reported, and "is the version I'm pinning past the known CVEs?" confirmation. Vuln DBs lag real-world attacks by days to weeks.

So for each new (or audited) dependency, **search the web** for recent (last ~6–12 mo) incidents: `"<package> supply chain attack OR malicious release OR maintainer compromise <year>"`, plus the package's release/security notes. Record a per-dep verdict: clean / incident-found+action / archived+migrate / version-confirmed-past-CVE.

> **Why this layer is mandatory:** in chain 297, `govulncheck` and `osv-scanner` were both green on the original dependency graph — yet the web scan surfaced an *archived* dependency (no future patches) and confirmed our SDK version was past two recent CVEs. Neither is a "current vuln" a scanner fails on. A green scanner is necessary, not sufficient.

### Transitive deps
Adding one package adds its whole subtree. After install, re-run layers 1–2 against the *new* lockfile and skim what got pulled in. A heavy tool's transitive deps become part of your vetted-and-pinned surface (or a reason to reject the tool).

## Pin to an exact version (never float)

Vetting a version means nothing if the version can drift. Pin per ecosystem, and commit the lockfile:

| Ecosystem | Pin mechanism |
|---|---|
| Go | exact version in `go.mod` + cryptographic hashes in `go.sum` (`go mod verify`); `-mod=readonly`; build/dev tools via the Go 1.24+ `tool` directive (`go get -tool foo@vX.Y.Z`) — see `go-conventions §Module dependency management` |
| npm / JS | committed `package-lock.json` / `pnpm-lock.yaml`; exact versions, no `^`/`~` for security-sensitive deps |
| Python | `poetry.lock` / `uv.lock` / hashed `requirements.txt` (`--require-hashes`) |
| Rust | committed `Cargo.lock`; exact or tilde as policy dictates |

**`@latest` belongs in throwaway shell history, not in a manifest.** Two machines running the same install must land the same bytes.

## The load-bearing reflexes

1. **Vet before install, not after.** The gate is upstream of the first `go get` — once it compiles, the bias is to keep it.
2. **Three layers, because each is blind to the others' failure modes.** A green `govulncheck` does NOT mean "safe" — it means "no *cataloged, reachable* vuln." Run OSV and the web scan anyway.
3. **Archived ≠ safe.** No-future-patches is a supply-chain risk; prefer the maintained fork.
4. **Pin, then commit the lockfile.** Unpinned vetting is theater.
5. **Tools are dependencies.** A linter / codegen / mutation tester runs with your privileges and pulls a subtree — vet and pin it like any library.

## Anti-patterns (auto-fail the discipline)

- **Install-then-vet** ("it compiles, I'll check later").
- **Trusting one scanner** — shipping because `npm audit` / `govulncheck` was green, skipping OSV + the web scan.
- **Floating versions** (`@latest`, unpinned `^`) on a security-sensitive dep.
- **Ignoring the transitive subtree** a new package or tool drags in.
- **Treating an archived/unmaintained package as fine** because it has no *current* CVE.
- **Skipping the manual web scan** because the automated scan was green — the exact gap that lets a fresh injection through.

## Acceptance: a dependency is admitted only when

- [ ] **Provenance + maintenance checked** (publisher known, not archived, import path verified — no typosquat).
- [ ] **Automated scan clean** across an ecosystem reachability tool AND a broad OSV scan (or every finding triaged).
- [ ] **Manual web scan done** — no recent supply-chain/maintainer-compromise/typosquat incident; version confirmed past any known CVE; verdict recorded.
- [ ] **Transitive subtree reviewed** (re-scan the new lockfile).
- [ ] **Pinned to an exact version + lockfile committed.**
- [ ] (Where enforced) the repo's vuln gate (`make vuln` in mcp-servers) passes.

If a layer legitimately doesn't apply (e.g. a stdlib-only package has a trivial subtree), say so explicitly. Silent skips are the anti-pattern.

## Boundary: this skill vs related skills

- **`go-conventions §Module dependency management`** — the Go *mechanisms* (`tool` directive, `go mod why -m`, pin-not-`@latest`, drop-silent-skip-once-module-managed). This skill is the cross-ecosystem *process* that wraps them; don't restate the Go specifics here.
- **`coding-philosophy`** — the regression-gate / no-escape-hatch philosophy this enforcement (the `make vuln` gate) is an instance of.
- **`bug-filing-discipline`** — a vetting finding worth tracking (a vulnerable dep we can't yet bump, an archived dep pending migration) is filed as a bug per that discipline.
- **`vault-filing-discipline`** — a cross-project supply-chain lesson (a class of incident, a tooling gap) warrants a vault note.

## Worked example (mcp-servers chain 297)

Audited 6 Go direct deps. `go mod tidy` no-op (surface minimal); `go mod verify` clean. Layer-2: `govulncheck` flagged one unreachable Windows-only vuln in `golang.org/x/sys` (bumped v0.41→v0.44 to clear); `osv-scanner` green. **Layer-3 web scan is what earned its keep:** found `gopkg.in/yaml.v3` archived/unmaintained (migrated to the maintained `go.yaml.in/yaml/v3` fork) and confirmed `modelcontextprotocol/go-sdk@v1.6.0` was past its two 2026 CVEs — both invisible to the scanners. Every remaining dep pinned (go.sum + `-mod=readonly`); enforcement wired as a `make vuln` precommit gate.
