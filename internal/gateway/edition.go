package gateway

import "net/http"

// ObserveEdition is set by `magpie observe` before the gateway listens.
// It turns off product surfaces that are not the proxy: shared memory,
// library skill distribution is skipped by the command, and the MCP
// servers for memory, search, vision, image, browser, plugin tools and
// the library hub answer 404. Protocol proxy routes are unchanged.
var ObserveEdition bool

func editionRefuses(w http.ResponseWriter, what string) bool {
	if !ObserveEdition {
		return false
	}
	http.Error(w, "this observe edition does not serve "+what, http.StatusNotFound)
	return true
}
