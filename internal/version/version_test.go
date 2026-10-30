package version

import (
	"runtime"
	"testing"
)

func TestGetReportsBuildInfo(t *testing.T) {
	set := Version
	defer func() { Version = set }()
	Version = "v9.9.9"
	got := Get()
	if got.Version != "v9.9.9" {
		t.Fatalf("Version = %q", got.Version)
	}
	if got.Go != runtime.Version() {
		t.Fatalf("Go = %q want %q", got.Go, runtime.Version())
	}
	if got.OS == "" || got.Arch == "" {
		t.Fatalf("missing platform: %+v", got)
	}
}

func TestDefaultVersionIsDev(t *testing.T) {
	if Version != "dev" && Version == "" {
		t.Fatal("version must never be empty")
	}
}
