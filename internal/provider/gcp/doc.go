// Package gcp implements muster's provider capabilities on Google Cloud,
// targeting stateful managed instance groups.
//
// It is compiled only under `-tags gcp`. This file carries no build constraint
// so that a plain `go build ./...` sees a valid (empty) package rather than
// failing with "build constraints exclude all Go files".
package gcp
