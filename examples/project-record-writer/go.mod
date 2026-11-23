module github.com/3leaps/recordprims/examples/project-record-writer

go 1.26.0

require (
	github.com/3leaps/canonprims v0.1.4
	github.com/3leaps/recordprims v0.0.0
)

require golang.org/x/sys v0.49.0 // indirect

replace github.com/3leaps/recordprims => ../..
