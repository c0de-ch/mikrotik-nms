package toolchain

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// minSetupGo is the first actions/setup-go major that installs the toolchain
// line of go.mod (v5 read only the go line) and sets GOTOOLCHAIN=local.
const minSetupGo = 6

var (
	toolchainLine = regexp.MustCompile(`(?m)^toolchain go(\S+)\s*$`)
	sha256Hex     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	golangStage   = regexp.MustCompile(`(?m)^FROM\s+(?:--\S+\s+)*golang:(\S+)`)
	setupGo       = regexp.MustCompile(`uses:\s*actions/setup-go@v(\d+)`)
	goVersionKey  = regexp.MustCompile(`(?m)^\s*go-version:`)
)

func TestPinnedEverywhere(t *testing.T) {
	backend := moduleRoot(t)
	repo := filepath.Dir(backend)

	m := toolchainLine.FindStringSubmatch(read(t, filepath.Join(backend, "go.mod")))
	if m == nil {
		t.Fatal("backend/go.mod has no toolchain line: it is the Go every build path pins")
	}
	want := m[1]
	bump := "bump backend/go.mod's toolchain line, the installers' GO_VERSION + GO_SHA256_* " +
		"(https://go.dev/dl/?mode=json) and the golang builder image together"

	for _, rel := range []string{"deploy/lxc/install.sh", "deploy/webhook-agent/install.sh"} {
		s := read(t, filepath.Join(repo, rel))
		if got := shellVar(s, "GO_VERSION"); got != want {
			t.Errorf("%s: GO_VERSION=%q, backend/go.mod pins go%s: %s", rel, got, want, bump)
		}
		for _, name := range []string{"GO_SHA256_AMD64", "GO_SHA256_ARM64"} {
			if got := shellVar(s, name); !sha256Hex.MatchString(got) {
				t.Errorf("%s: %s=%q is not a sha256: the archive of go%s is verified before it is installed", rel, name, got, want)
			}
		}
	}

	dockerfiles, err := filepath.Glob(filepath.Join(repo, "deploy", "docker", "Dockerfile*"))
	if err != nil {
		t.Fatal(err)
	}
	stages := 0
	for _, path := range dockerfiles {
		s := read(t, path)
		rel := relTo(repo, path)
		for _, m := range golangStage.FindAllStringSubmatch(s, -1) {
			stages++
			if tag := m[1]; tag != want && !strings.HasPrefix(tag, want+"-") {
				t.Errorf("%s: builder image golang:%s, backend/go.mod pins go%s: %s", rel, tag, want, bump)
			}
		}
		if golangStage.MatchString(s) && !strings.Contains(s, "ENV GOTOOLCHAIN=local") {
			t.Errorf("%s: a golang stage without ENV GOTOOLCHAIN=local may download a toolchain instead of failing", rel)
		}
	}
	if stages == 0 {
		t.Error("no golang builder stage in deploy/docker/Dockerfile*: the backend image pins the Go it is built with")
	}

	workflows, err := filepath.Glob(filepath.Join(repo, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range workflows {
		s := read(t, path)
		rel := relTo(repo, path)
		uses := setupGo.FindAllStringSubmatch(s, -1)
		for _, m := range uses {
			if major, _ := strconv.Atoi(m[1]); major < minSetupGo {
				t.Errorf("%s: actions/setup-go@v%s does not install go.mod's toolchain line; use v%d or later", rel, m[1], minSetupGo)
			}
		}
		if len(uses) > 0 && (goVersionKey.MatchString(s) || !strings.Contains(s, "go-version-file: backend/go.mod")) {
			t.Errorf("%s: setup-go takes the Go version from backend/go.mod (go-version-file), not from a go-version of its own", rel)
		}
	}
}

// shellVar is the value of a top-level NAME="value" assignment in a shell
// script, or "" when the script has none.
func shellVar(script, name string) string {
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `="([^"]*)"`).FindStringSubmatch(script)
	if m == nil {
		return ""
	}
	return m[1]
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("toolchain: no go.mod above the test directory")
		}
		dir = parent
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func relTo(base, path string) string {
	if rel, err := filepath.Rel(base, path); err == nil {
		return filepath.ToSlash(rel)
	}
	return path
}
