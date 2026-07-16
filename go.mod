module corpos

go 1.26.3

// Build with go1.26.4+: go1.26.3's stdlib carries GO-2026-5039 (net/textproto)
// and GO-2026-5037 (crypto/x509), both fixed in go1.26.4 — the gate's govulncheck
// flags them otherwise. See bug corpos-gate-broken-stdlib-vulns-go1264.
toolchain go1.26.5

require (
	github.com/BurntSushi/toml v1.6.0
	modernc.org/sqlite v1.51.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.44.0 // indirect
	modernc.org/libc v1.72.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
