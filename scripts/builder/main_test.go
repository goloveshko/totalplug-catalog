package main

import (
	"os"
	"path/filepath"
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
