package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDetectViteWithYarn(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"^5.0.0"}}`,
		"yarn.lock":    "",
	})
	p := Detect(dir)
	if p.Framework != "vite" || p.Runner != "yarn" || p.DevCommand != "yarn dev" || p.Port != 5173 {
		t.Errorf("got %+v", p)
	}
}

func TestDetectNextTakesPrecedenceOverVite(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"package.json": `{"scripts":{"dev":"next dev"},"dependencies":{"next":"14.0.0"},"devDependencies":{"vite":"^5.0.0"}}`,
	})
	p := Detect(dir)
	if p.Framework != "next" || p.Port != 3000 {
		t.Errorf("got %+v", p)
	}
}

func TestDetectStaticSite(t *testing.T) {
	dir := writeProject(t, map[string]string{"index.html": "<!doctype html>"})
	p := Detect(dir)
	if p.Framework != "static" || p.BuildDir != "." {
		t.Errorf("got %+v", p)
	}
}

func TestDetectUnknownFallsBackToBaseURL(t *testing.T) {
	p := Detect(t.TempDir())
	if p.Framework != "unknown" {
		t.Errorf("got %+v", p)
	}
	cfg := Render(p)
	if !strings.Contains(cfg, "baseUrl:") {
		t.Errorf("unknown project config missing baseUrl hint:\n%s", cfg)
	}
}

func TestRenderedConfigMentionsBaseline(t *testing.T) {
	cfg := Render(Detect(t.TempDir()))
	if !strings.Contains(cfg, "--update-baseline") {
		t.Errorf("starter config must teach the baseline workflow")
	}
}
