// Package toolchain pins, as a test, the Go every build path uses: the
// toolchain line of backend/go.mod. The installers (deploy/lxc and
// deploy/webhook-agent: GO_VERSION and the archives' sha256), the backend
// image's builder stage and CI (setup-go reading go.mod) each name it on
// their own, so a bump of one without the others fails `go test`.
package toolchain
