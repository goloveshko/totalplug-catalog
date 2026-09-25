package main

import (
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
