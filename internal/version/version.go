// Package version describes the running build: its release number, commit,
// and the Go and disgo versions it was built with. All of it comes from the
// build info Go embeds in every binary; release numbers come from git tags
// (v1.0.0): a build of a tagged commit carries that tag as its version.
package version

import (
	"fmt"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// Info is what one build knows about itself.
type Info struct {
	Release  string    // e.g. "v1.0.0"; "" unless built from exactly a tagged commit
	After    string    // for an untagged build, the last release before it ("" if none)
	Commit   string    // short commit hash; "" without git info (e.g. go run)
	Time     time.Time // when that commit was made
	Modified bool      // built with uncommitted changes
	Go       string    // e.g. "1.27.1"
	Disgo    string    // e.g. "v0.19.6"
}

// Read returns the running binary's Info.
func Read() Info {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return Info{}
	}
	return FromBuildInfo(bi)
}

// pseudoTail matches the end of Go's pseudo-versions, which it stamps on
// builds that aren't exactly a tagged commit. What comes before it names the
// last release, in one of three forms:
//
//	v0.0.0-20261004111702-20e2e37bac69          no tag yet
//	v1.0.1-0.20261004111702-20e2e37bac69        after v1.0.0 (patch + 1)
//	v1.1.0-rc.1.0.20261004111702-20e2e37bac69   after v1.1.0-rc.1
var pseudoTail = regexp.MustCompile(`[-.](0\.)?\d{14}-[0-9a-f]{12}$`)

// release matches a plain vMAJOR.MINOR.PATCH.
var release = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// FromBuildInfo reads Info from bi.
func FromBuildInfo(bi *debug.BuildInfo) Info {
	i := Info{Go: strings.TrimPrefix(bi.GoVersion, "go")}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			i.Commit = s.Value[:min(len(s.Value), 7)]
		case "vcs.time":
			i.Time, _ = time.Parse(time.RFC3339, s.Value)
		case "vcs.modified":
			i.Modified = s.Value == "true"
		}
	}
	for _, d := range bi.Deps {
		if d.Path == "github.com/disgoorg/disgo" {
			i.Disgo = d.Version
		}
	}
	v, dirty := strings.CutSuffix(bi.Main.Version, "+dirty")
	i.Modified = i.Modified || dirty
	if v == "" || v == "(devel)" {
		return i
	}
	loc := pseudoTail.FindStringSubmatchIndex(v)
	if loc == nil {
		i.Release = v
		return i
	}
	if loc[2] < 0 { // no "0.": no tag before this build
		return i
	}
	base := v[:loc[0]]
	if v[loc[0]] == '.' { // after a pre-release: it's the base itself
		i.After = base
		return i
	}
	if m := release.FindStringSubmatch(base); m != nil { // vX.Y.(Z+1): after vX.Y.Z
		if patch, err := strconv.Atoi(m[3]); err == nil && patch > 0 {
			i.After = fmt.Sprintf("v%s.%s.%d", m[1], m[2], patch-1)
		}
	}
	return i
}

// Name is the build's version for people: the release, or what it's based on.
func (i Info) Name() string {
	switch {
	case i.Release != "":
		return i.Release
	case i.Commit == "":
		return "dev build"
	case i.After != "":
		return "unreleased (after " + i.After + ")"
	}
	return "unreleased"
}

// String is one line for logs and the owner's debug command, e.g.
// "v1.0.0 · commit 20e2e37 (2026-10-04 11:17 UTC) · Go 1.27.1 · disgo v0.19.6".
func (i Info) String() string {
	parts := []string{i.Name()}
	if i.Commit != "" {
		c := "commit " + i.Commit
		if !i.Time.IsZero() {
			c += " (" + i.Time.UTC().Format("2006-01-02 15:04 MST") + ")"
		}
		if i.Modified {
			c += " + local changes"
		}
		parts = append(parts, c)
	}
	if i.Go != "" {
		parts = append(parts, "Go "+i.Go)
	}
	if i.Disgo != "" {
		parts = append(parts, "disgo "+i.Disgo)
	}
	return strings.Join(parts, " · ")
}
