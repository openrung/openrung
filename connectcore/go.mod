module github.com/openrung/openrung/connectcore

go 1.25.0

require (
	github.com/gorilla/websocket v1.5.3
	github.com/openrung/openrung/brokerapi v0.6.0
	github.com/openrung/openrung/punchcore v0.1.0
	github.com/openrung/openrung/wsscore v0.7.0
	golang.org/x/sys v0.45.0
)

require github.com/hashicorp/yamux v0.1.2 // indirect

// In-repo builds compile the sibling modules from source, like every other
// consumer in this repository. A fetched connectcore ignores these directives
// (replace applies only in the main module) and resolves the required
// versions above, so a connectcore change that needs new sibling-module API
// must bump that sibling's VERSION and this require list in the same PR.
replace github.com/openrung/openrung/brokerapi => ../brokerapi

replace github.com/openrung/openrung/punchcore => ../punchcore

replace github.com/openrung/openrung/wsscore => ../wsscore
