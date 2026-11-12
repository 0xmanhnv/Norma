module github.com/0xmanhnv/corvid

go 1.26.11

require (
	github.com/creack/pty v1.1.24
	github.com/hinshun/vt10x v0.0.0-20220301184237-5011da428d02
	golang.org/x/net v0.62.0
)

require gopkg.in/yaml.v3 v3.0.1

// Versions published before this module had its own version line. Their
// go.mod declares another module path (github.com/Autumn-27/norma or
// github.com/0xmanhnv/norma), so they cannot be required under this path.
// The fork's own line starts at v0.5.0.
retract (
	v0.4.4 // upstream's release tag; go.mod declares github.com/Autumn-27/norma
	[v0.1.0, v0.4.4-p.4] // inherited tags; go.mod declares an earlier module path
)
