package version

import (
	"runtime/debug"
	"testing"
)

func info(mainVersion string, modified bool) *debug.BuildInfo {
	mod := "false"
	if modified {
		mod = "true"
	}
	bi := &debug.BuildInfo{GoVersion: "go1.27.1", Main: debug.Module{Version: mainVersion},
		Deps: []*debug.Module{{Path: "github.com/disgoorg/disgo", Version: "v0.19.6"}}}
	if mainVersion != "(devel)" {
		bi.Settings = []debug.BuildSetting{
			{Key: "vcs.revision", Value: "20e2e37bac69a6ef2a0faa2041250f4520ac7fb1"},
			{Key: "vcs.time", Value: "2026-10-04T11:17:02Z"},
			{Key: "vcs.modified", Value: mod},
		}
	}
	return bi
}

func TestVersionText(t *testing.T) {
	const tail = " · Go 1.27.1 · disgo v0.19.6"
	cases := []struct {
		name, main string
		modified   bool
		want       string
	}{
		{"tagged", "v1.0.0", false, "v1.0.0 · commit 20e2e37 (2026-10-04 11:17 UTC)" + tail},
		{"tagged, edited", "v1.0.0+dirty", true, "v1.0.0 · commit 20e2e37 (2026-10-04 11:17 UTC) + local changes" + tail},
		{"after a release", "v1.0.1-0.20261004111702-20e2e37bac69", false, "unreleased (after v1.0.0) · commit 20e2e37 (2026-10-04 11:17 UTC)" + tail},
		{"after a minor release", "v1.2.1-0.20261004111702-20e2e37bac69+dirty", false, "unreleased (after v1.2.0) · commit 20e2e37 (2026-10-04 11:17 UTC) + local changes" + tail},
		{"no tags yet", "v0.0.0-20261004111702-20e2e37bac69", false, "unreleased · commit 20e2e37 (2026-10-04 11:17 UTC)" + tail},
		{"pre-release tag", "v1.1.0-rc.1", false, "v1.1.0-rc.1 · commit 20e2e37 (2026-10-04 11:17 UTC)" + tail},
		{"after a pre-release", "v1.1.0-rc.1.0.20261004111702-20e2e37bac69", false, "unreleased (after v1.1.0-rc.1) · commit 20e2e37 (2026-10-04 11:17 UTC)" + tail},
		{"go run", "(devel)", false, "dev build" + tail},
	}
	for _, c := range cases {
		if got := FromBuildInfo(info(c.main, c.modified)).String(); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
	if got := (Info{}).String(); got != "dev build" {
		t.Errorf("no build info: %q", got)
	}
}
