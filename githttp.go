package main

import "net/http"

// registerGitRoutes adds the endpoints that change the commits of a
// checkout.
func registerGitRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/commits/message", handleCommitMessage)
}
