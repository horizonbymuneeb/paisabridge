// Package version exposes the build identity of the binary.
//
// The value is injected at link time:
//
//	go build -ldflags "-X github.com/horizonbymuneeb/paisabridge/internal/version.Version=v1.2.3"
//
// and falls back to "dev" for a plain `go build`, so /api/healthz always
// identifies a rollout without the operator having to guess.
package version

import "runtime"

// Version is the release tag, or "dev" for a source build.
var Version = "dev"

// Info is the payload reported by the health endpoint.
type Info struct {
	Version string `json:"version"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// Get returns the build info.
func Get() Info {
	return Info{Version: Version, Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
}
