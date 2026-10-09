package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		v1, v2 string
		want   int
	}{
		{"1.0", "1.0", 0},
		{"1.2", "1.10", -1},
		{"1.10", "1.2", 1},
		{"2.3", "2.3.1", -1},
		{"v2.0", "1.9.9", 1},
		{"1_0_5", "1.0.5", 0},
		{"3", "2.9.9", 1},
		{"", "1", -1},
	}

	for _, tt := range tests {
		if got := CompareVersions(tt.v1, tt.v2); got != tt.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.v1, tt.v2, got, tt.want)
		}
	}
}

func TestParseTotalCmdDate(t *testing.T) {
	want := time.Date(2006, 1, 2, 0, 0, 0, 0, time.UTC)

	for _, raw := range []string{"02.01.2006", "2.01.2006", "2.1.2006", "02.1.2006", "2006-01-02", "02-01-2006", " 2.1.2006 "} {
		if got := parseTotalCmdDate(raw); !got.Equal(want) {
			t.Errorf("parseTotalCmdDate(%q) = %v, want %v", raw, got, want)
		}
	}

	for _, raw := range []string{"", "   ", "02/01/2006", "not a date", "01.02.2006.1"} {
		if got := parseTotalCmdDate(raw); !got.IsZero() {
			t.Errorf("parseTotalCmdDate(%q) = %v, want zero time", raw, got)
		}
	}
}

func TestParseTotalCmdArch(t *testing.T) {
	tests := []struct {
		raw           string
		wantArch      string
		wantHasSource bool
	}{
		{"x32", "x32", false},
		{"x64", "x64", false},
		{"x32+x64", "x32+x64", false},
		{"x64+src", "x64", true},
		{"x32+src+x64", "x32+x64", true},
		{"", "any", false},
		{"  ", "any", false},
	}

	for _, tt := range tests {
		arch, hasSource := parseTotalCmdArch(tt.raw)
		if arch != tt.wantArch || hasSource != tt.wantHasSource {
			t.Errorf("parseTotalCmdArch(%q) = (%q, %v), want (%q, %v)",
				tt.raw, arch, hasSource, tt.wantArch, tt.wantHasSource)
		}
	}
}

func TestClassifyPluginType(t *testing.T) {
	tests := []struct {
		category string
		want     string
	}{
		{"packer", "WCX"},
		{"multiarc", "WCX"},
		{"lister", "WLX"},
		{"Viewer", "WLX"},
		{"fsplugin", "WFX"},
		{"FS", "WFX"},
		{"content", "WDX"},
		{"synplus", "WDX"},
		{"lang", "LANG"},
		{"iconpack", "UTIL"},
		{"other", "UTIL"},
		{"", "UTIL"},
		{" fsplugin ", "WFX"},
	}

	for _, tt := range tests {
		if got := classifyPluginType(tt.category); got != tt.want {
			t.Errorf("classifyPluginType(%q) = %q, want %q", tt.category, got, tt.want)
		}
	}
}

func TestDedupeStrings(t *testing.T) {
	got := dedupeStrings([]string{"calendar", "calendar", " other "})
	want := []string{"calendar", " other "}
	if len(got) != len(want) {
		t.Fatalf("dedupeStrings() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedupeStrings() = %v, want %v", got, want)
		}
	}
}

func TestDropRedundantSources(t *testing.T) {
	resolved := ResolvedSource{Version: "2.0", DownloadURL: "https://example.com/gh.zip"}

	tests := []struct {
		name     string
		entry    CatalogEntry
		wantKeys []string
	}{
		{
			name: "single echo source is dropped",
			entry: CatalogEntry{
				Resolved: &resolved,
				AvailableSources: map[string]ResolvedSource{
					"github": resolved,
				},
			},
			wantKeys: nil,
		},
		{
			name: "alternative source is kept",
			entry: CatalogEntry{
				Resolved: &resolved,
				AvailableSources: map[string]ResolvedSource{
					"github":   resolved,
					"totalcmd": {Version: "1.9", DownloadURL: "https://totalcmd.net/download.php?id=x"},
				},
			},
			wantKeys: []string{"totalcmd"},
		},
		{
			name: "same url but newer version is kept",
			entry: CatalogEntry{
				Resolved: &resolved,
				AvailableSources: map[string]ResolvedSource{
					"mirror": {Version: "2.1", DownloadURL: resolved.DownloadURL},
				},
			},
			wantKeys: []string{"mirror"},
		},
		{
			name: "source with empty version is treated as identical",
			entry: CatalogEntry{
				Resolved: &resolved,
				AvailableSources: map[string]ResolvedSource{
					"totalcmd": {Version: "", DownloadURL: resolved.DownloadURL},
				},
			},
			wantKeys: nil,
		},
		{
			name: "nothing dropped when resolved is nil",
			entry: CatalogEntry{
				Resolved: nil,
				AvailableSources: map[string]ResolvedSource{
					"totalcmd": {Version: "1.0", DownloadURL: "u"},
				},
			},
			wantKeys: []string{"totalcmd"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := tt.entry
			dropRedundantSources(&entry)

			if tt.wantKeys == nil {
				if len(entry.AvailableSources) != 0 {
					t.Fatalf("AvailableSources = %v, want empty or nil", entry.AvailableSources)
				}
				return
			}
			if len(entry.AvailableSources) != len(tt.wantKeys) {
				t.Fatalf("AvailableSources = %v, want keys %v", entry.AvailableSources, tt.wantKeys)
			}
			for _, k := range tt.wantKeys {
				if _, ok := entry.AvailableSources[k]; !ok {
					t.Errorf("expected source %q to be kept, got %v", k, entry.AvailableSources)
				}
			}
		})
	}
}

func TestLoadCommunityManifests(t *testing.T) {
	writeManifest := func(t *testing.T, dir, file, id string) {
		t.Helper()
		content := `{"id":"` + id + `","name":"` + id + `","type":"WLX","description":"d","match":{"filenames":["a.wlx"]},"source":{"type":"direct_url","download_url":"https://example.com/a.zip","version":"1.0"}}`
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("loads valid manifests", func(t *testing.T) {
		dir := t.TempDir()
		writeManifest(t, dir, "one.json", "one")
		writeManifest(t, dir, "two.json", "two")

		got, err := loadCommunityManifests(dir)
		if err != nil || len(got) != 2 {
			t.Fatalf("loadCommunityManifests() = %d manifests, err %v; want 2, nil", len(got), err)
		}
	})

	t.Run("duplicate id fails", func(t *testing.T) {
		dir := t.TempDir()
		writeManifest(t, dir, "one.json", "same")
		writeManifest(t, dir, "two.json", "same")

		if _, err := loadCommunityManifests(dir); err == nil {
			t.Fatal("expected error for duplicate id, got nil")
		}
	})

	t.Run("reserved totalcmd_ prefix fails", func(t *testing.T) {
		dir := t.TempDir()
		writeManifest(t, dir, "one.json", "totalcmd_calendar")

		if _, err := loadCommunityManifests(dir); err == nil {
			t.Fatal("expected error for reserved prefix, got nil")
		}
	})

	t.Run("missing dir is not an error", func(t *testing.T) {
		got, err := loadCommunityManifests(filepath.Join(t.TempDir(), "nope"))
		if err != nil || len(got) != 0 {
			t.Fatalf("loadCommunityManifests() = %d manifests, err %v; want 0, nil", len(got), err)
		}
	})

	writeTypedManifest := func(t *testing.T, dir, file, id, typ string) {
		t.Helper()
		content := `{"id":"` + id + `","name":"` + id + `","type":"` + typ +
			`","description":"d","match":{"filenames":["a.wlx"]},"source":{"type":"direct_url","download_url":"https://example.com/a.zip","version":"1.0"}}`
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("type matching its directory loads", func(t *testing.T) {
		root := t.TempDir()
		sub := filepath.Join(root, "wlx")
		if err := os.Mkdir(sub, 0755); err != nil {
			t.Fatal(err)
		}
		writeTypedManifest(t, sub, "foo.json", "foo", "WLX")

		got, err := loadCommunityManifests(root)
		if err != nil || len(got) != 1 {
			t.Fatalf("loadCommunityManifests() = %d manifests, err %v; want 1, nil", len(got), err)
		}
	})

	t.Run("type not matching its directory fails", func(t *testing.T) {
		root := t.TempDir()
		sub := filepath.Join(root, "wcx")
		if err := os.Mkdir(sub, 0755); err != nil {
			t.Fatal(err)
		}
		writeTypedManifest(t, sub, "foo.json", "foo", "WLX")

		if _, err := loadCommunityManifests(root); err == nil {
			t.Fatal("expected error for type/directory mismatch, got nil")
		}
	})
}

func TestPickPreferredLegacy(t *testing.T) {
	entry := func(id, ver string) CatalogEntry {
		return CatalogEntry{ID: id, Resolved: &ResolvedSource{Version: ver}}
	}

	t.Run("empty returns nil", func(t *testing.T) {
		if got := pickPreferredLegacy(nil); got != nil {
			t.Fatalf("pickPreferredLegacy(nil) = %v, want nil", got)
		}
	})

	t.Run("highest version wins", func(t *testing.T) {
		matches := []CatalogEntry{entry("totalcmd_a", "1.0"), entry("totalcmd_b", "2.5"), entry("totalcmd_c", "2.0")}
		got := pickPreferredLegacy(matches)
		if got == nil || got.ID != "totalcmd_b" {
			t.Fatalf("pickPreferredLegacy() = %v, want totalcmd_b", got)
		}
	})

	t.Run("version tie breaks on lowest id", func(t *testing.T) {
		matches := []CatalogEntry{entry("totalcmd_zulu", "3.0"), entry("totalcmd_alpha", "3.0")}
		got := pickPreferredLegacy(matches)
		if got == nil || got.ID != "totalcmd_alpha" {
			t.Fatalf("pickPreferredLegacy() = %v, want totalcmd_alpha", got)
		}
	})
}

func TestDetectArchFromAssets(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		want  string
	}{
		{"universal package", []string{"plugin.zip"}, "x32+x64"},
		{"empty list", nil, "x32+x64"},
		{"x64 only", []string{"tool-x64.zip"}, "x64"},
		{"win64 only", []string{"tool-win64.zip"}, "x64"},
		{"x86_64 is 64-bit", []string{"tool-x86_64.zip"}, "x64"},
		{"amd64", []string{"tool.amd64.zip"}, "x64"},
		{"win32 only", []string{"tool-win32.zip"}, "x32"},
		{"x86 is 32-bit", []string{"tool-x86.zip"}, "x32"},
		{"i686", []string{"tool.i686.zip"}, "x32"},
		{"both split", []string{"tool-win32.zip", "tool-win64.zip"}, "x32+x64"},
		{"release with source zip", []string{"plugin-1.2.zip", "source code.zip"}, "x32+x64"},
		{"x64 release plus source", []string{"plugin-x64.zip", "Source code.zip"}, "x64"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectArchFromAssets(tt.names); got != tt.want {
				t.Errorf("detectArchFromAssets(%v) = %q, want %q", tt.names, got, tt.want)
			}
		})
	}
}

func TestNormalizeStr(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"wlx-edge-viewer", "wlxedgeviewer"},
		{"Cuda_Lister", "cudalister"},
		{"PAXZ", "paxz"},
	}

	for _, tt := range tests {
		if got := normalizeStr(tt.in); got != tt.want {
			t.Errorf("normalizeStr(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPickPrimarySource(t *testing.T) {
	src := func(v string) ResolvedSource { return ResolvedSource{Version: v} }

	t.Run("nil current picks highest, deterministic on tie", func(t *testing.T) {
		sources := map[string]ResolvedSource{
			"github":   src("2.0"),
			"totalcmd": src("2.0"),
		}
		// Equal versions: sorted-name order makes "github" win every run.
		for i := 0; i < 20; i++ {
			got, from := pickPrimarySource(nil, sources)
			if got == nil || got.Version != "2.0" || from != "github" {
				t.Fatalf("pickPrimarySource() = (%v, %q), want (2.0, github)", got, from)
			}
		}
	})

	t.Run("higher version source is promoted", func(t *testing.T) {
		current := src("1.0")
		sources := map[string]ResolvedSource{"github": src("1.5")}
		got, from := pickPrimarySource(&current, sources)
		if got.Version != "1.5" || from != "github" {
			t.Fatalf("pickPrimarySource() = (%v, %q), want (1.5, github)", got, from)
		}
	})

	t.Run("current wins when no source is higher", func(t *testing.T) {
		current := src("3.0")
		sources := map[string]ResolvedSource{"github": src("2.0"), "mirror": src("3.0")}
		got, from := pickPrimarySource(&current, sources)
		if got != &current || from != "" {
			t.Fatalf("pickPrimarySource() = (%v, %q), want unchanged current and empty name", got, from)
		}
	})

	t.Run("empty sources keep current", func(t *testing.T) {
		current := src("1.0")
		got, from := pickPrimarySource(&current, nil)
		if got != &current || from != "" {
			t.Fatalf("pickPrimarySource() = (%v, %q), want unchanged current", got, from)
		}
	})

	t.Run("nil current and no sources", func(t *testing.T) {
		got, from := pickPrimarySource(nil, nil)
		if got != nil || from != "" {
			t.Fatalf("pickPrimarySource() = (%v, %q), want (nil, empty)", got, from)
		}
	})
}

func TestSelectAssetURL(t *testing.T) {
	assets := []GitHubAsset{
		{Name: "plugin-1.0-win32.zip", BrowserDownloadURL: "u/win32.zip"},
		{Name: "plugin-1.0-x64.zip", BrowserDownloadURL: "u/x64.zip"},
		{Name: "Source code.zip", BrowserDownloadURL: "u/source.zip"},
	}

	t.Run("nil pattern prefers first archive", func(t *testing.T) {
		if got := selectAssetURL(assets, nil); got != "u/win32.zip" {
			t.Errorf("selectAssetURL() = %q, want u/win32.zip", got)
		}
	})

	t.Run("pattern matches specific asset", func(t *testing.T) {
		re := regexp.MustCompile(`x64\.zip$`)
		if got := selectAssetURL(assets, re); got != "u/x64.zip" {
			t.Errorf("selectAssetURL() = %q, want u/x64.zip", got)
		}
	})

	t.Run("unmatched pattern yields empty, no fallback", func(t *testing.T) {
		re := regexp.MustCompile(`\.msi$`)
		if got := selectAssetURL(assets, re); got != "" {
			t.Errorf("selectAssetURL() = %q, want empty (no assets[0] fallback)", got)
		}
	})

	t.Run("nil pattern falls back to first asset when no archive", func(t *testing.T) {
		nonArchive := []GitHubAsset{{Name: "checksum.txt", BrowserDownloadURL: "u/checksum.txt"}}
		if got := selectAssetURL(nonArchive, nil); got != "u/checksum.txt" {
			t.Errorf("selectAssetURL() = %q, want u/checksum.txt", got)
		}
	})

	t.Run("empty assets", func(t *testing.T) {
		if got := selectAssetURL(nil, nil); got != "" {
			t.Errorf("selectAssetURL() = %q, want empty", got)
		}
	})
}
