package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/transform"
)

const (
	TotalCmdListURL = "https://totalcmd.net/get_plugins_list.php"
	HTTPTimeout     = 15 * time.Second
)

// MatchRule defines how TotalPlug matches local files to catalog items.
type MatchRule struct {
	Aliases   []string `json:"aliases,omitempty"`
	Filenames []string `json:"filenames"`
}

// SourceInfo defines where to retrieve updates.
type SourceInfo struct {
	Type         string `json:"type"` // "github_release", "direct_url", "totalcmd_net"
	Repo         string `json:"repo,omitempty"`
	AssetPattern string `json:"asset_pattern,omitempty"`
	Version      string `json:"version,omitempty"`
	DownloadURL  string `json:"download_url,omitempty"`
	TotalcmdID   string `json:"totalcmd_id,omitempty"`
}

// PluginManifest represents a community-curated JSON manifest.
type PluginManifest struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Type        string     `json:"type"` // "WCX", "WLX", "WFX", "WDX"
	Category    string     `json:"category,omitempty"`
	Description string     `json:"description,omitempty"`
	Authors     []string   `json:"authors,omitempty"`
	Homepage    string     `json:"homepage,omitempty"`
	License     string     `json:"license,omitempty"`
	Match       MatchRule  `json:"match"`
	Source      SourceInfo `json:"source"`
}

// ResolvedSource holds dynamically fetched version and download links.
type ResolvedSource struct {
	Version      string    `json:"version"`
	DownloadURL  string    `json:"download_url"`
	PublishedAt  time.Time `json:"published_at,omitempty"`
	ResolvedFrom string    `json:"resolved_from"` // "github", "direct", "totalcmd"
}

// CatalogEntry represents the final merged entry in catalog.resolved.json.
type CatalogEntry struct {
	ID               string                    `json:"id"`
	Name             string                    `json:"name"`
	Type             string                    `json:"type"`
	Category         string                    `json:"category,omitempty"`
	Description      string                    `json:"description,omitempty"`
	Authors          []string                  `json:"authors,omitempty"`
	Homepage         string                    `json:"homepage,omitempty"`
	License          string                    `json:"license,omitempty"`
	Match            MatchRule                 `json:"match"`
	Source           SourceInfo                `json:"source"`
	Resolved         *ResolvedSource           `json:"resolved,omitempty"`
	AvailableSources map[string]ResolvedSource `json:"available_sources,omitempty"`
}

// CatalogSnapshot is the top-level payload served over CDN.
type CatalogSnapshot struct {
	Version     int            `json:"version"`
	GeneratedAt time.Time      `json:"generated_at"`
	TotalCount  int            `json:"total_count"`
	Plugins     []CatalogEntry `json:"plugins"`
}

// GitHubRelease represents official GitHub API response.
type GitHubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	PublishedAt time.Time     `json:"published_at"`
	Assets      []GitHubAsset `json:"assets"`
}

type GitHubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

func parseVersionNumbers(v string) []int {
	var parts []int
	var current int
	var hasNum bool

	for _, ch := range v {
		if ch >= '0' && ch <= '9' {
			current = current*10 + int(ch-'0')
			hasNum = true
		} else if ch == '.' || ch == '-' || ch == '_' {
			if hasNum {
				parts = append(parts, current)
				current = 0
				hasNum = false
			}
		}
	}
	if hasNum {
		parts = append(parts, current)
	}
	return parts
}

func CompareVersions(v1, v2 string) int {
	p1 := parseVersionNumbers(v1)
	p2 := parseVersionNumbers(v2)

	maxLen := len(p1)
	if len(p2) > maxLen {
		maxLen = len(p2)
	}

	for i := 0; i < maxLen; i++ {
		var n1, n2 int
		if i < len(p1) {
			n1 = p1[i]
		}
		if i < len(p2) {
			n2 = p2[i]
		}

		if n1 < n2 {
			return -1
		}
		if n1 > n2 {
			return 1
		}
	}

	return 0
}

func fetchTotalCmdBaseList(ctx context.Context) ([]CatalogEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, TotalCmdListURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", "TotalPlug-Catalog-Builder/1.0")

	client := &http.Client{Timeout: HTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %s", resp.Status)
	}

	// Decode Windows-1251 (CP1251) stream into UTF-8 on the fly:
	utf8Reader := transform.NewReader(resp.Body, charmap.Windows1251.NewDecoder())

	var list []CatalogEntry
	scanner := bufio.NewScanner(utf8Reader)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		parts := strings.Split(line, "|")
		if len(parts) < 6 {
			continue
		}

		id := strings.TrimSpace(parts[0])
		title := strings.TrimSpace(parts[1])
		ver := strings.TrimSpace(parts[2])
		category := strings.TrimSpace(parts[4])

		pType := "WLX"
		switch strings.ToUpper(category) {
		case "PACKER", "WCX":
			pType = "WCX"
		case "LISTER", "WLX":
			pType = "WLX"
		case "FS", "WFX", "FILE SYSTEM":
			pType = "WFX"
		case "CONTENT", "WDX":
			pType = "WDX"
		}

		resolved := ResolvedSource{
			Version:      ver,
			DownloadURL:  fmt.Sprintf("https://totalcmd.net/download.php?id=%s", id),
			ResolvedFrom: "totalcmd",
		}

		entry := CatalogEntry{
			ID:       fmt.Sprintf("totalcmd_%s", id),
			Name:     title,
			Type:     pType,
			Category: category,
			Match: MatchRule{
				Aliases:   []string{strings.ToLower(title), id},
				Filenames: []string{fmt.Sprintf("%s.%s", strings.ToLower(id), strings.ToLower(pType))},
			},
			Source: SourceInfo{
				Type:        "totalcmd_net",
				TotalcmdID:  id,
				DownloadURL: fmt.Sprintf("https://totalcmd.net/download.php?id=%s", id),
			},
			Resolved: &resolved,
			AvailableSources: map[string]ResolvedSource{
				"totalcmd": resolved,
			},
		}

		list = append(list, entry)
	}

	return list, nil
}

func loadCommunityManifests(pluginsDir string) ([]PluginManifest, error) {
	var manifests []PluginManifest

	err := filepath.Walk(pluginsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(strings.ToLower(info.Name()), ".json") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}

		var m PluginManifest
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}

		manifests = append(manifests, m)
		return nil
	})

	if err != nil {
		return nil, err
	}
	return manifests, nil
}

func resolveGitHubRelease(ctx context.Context, repo string, pattern string, ghToken string) (*ResolvedSource, error) {
	repo = strings.TrimPrefix(repo, "https://github.com/")
	repo = strings.TrimSuffix(repo, "/")

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "TotalPlug-Catalog-Builder/1.0")
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	if ghToken != "" {
		req.Header.Set("Authorization", "Bearer "+ghToken)
	}

	client := &http.Client{Timeout: HTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github api error (%s): %s", resp.Status, string(body))
	}

	var rel GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}

	cleanVer := strings.TrimPrefix(rel.TagName, "v")

	var downloadURL string
	var compiledRegexp *regexp.Regexp
	if pattern != "" {
		compiledRegexp, _ = regexp.Compile(pattern)
	}

	for _, asset := range rel.Assets {
		if compiledRegexp != nil {
			if compiledRegexp.MatchString(asset.Name) {
				downloadURL = asset.BrowserDownloadURL
				break
			}
		} else if strings.HasSuffix(strings.ToLower(asset.Name), ".zip") || strings.HasSuffix(strings.ToLower(asset.Name), ".rar") {
			downloadURL = asset.BrowserDownloadURL
			break
		}
	}

	if downloadURL == "" && len(rel.Assets) > 0 {
		downloadURL = rel.Assets[0].BrowserDownloadURL
	}

	return &ResolvedSource{
		Version:      cleanVer,
		DownloadURL:  downloadURL,
		PublishedAt:  rel.PublishedAt,
		ResolvedFrom: "github",
	}, nil
}

func main() {
	pluginsDir := flag.String("plugins", "plugins", "Path to community plugins directory")
	outDir := flag.String("out", "dist", "Output directory for compiled catalog")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := context.Background()
	ghToken := os.Getenv("GITHUB_TOKEN")

	logger.Info("Starting TotalPlug catalog build process...")

	// 1. Load community manifests
	logger.Info("Loading community plugins...", "dir", *pluginsDir)
	communityManifests, err := loadCommunityManifests(*pluginsDir)
	if err != nil {
		logger.Warn("Failed to load community plugins (or folder is empty)", "err", err)
	} else {
		logger.Info("Discovered community manifests", "count", len(communityManifests))
	}

	// 2. Fetch base totalcmd.net catalog
	logger.Info("Fetching base totalcmd.net feed...")
	baseEntries, err := fetchTotalCmdBaseList(ctx)
	if err != nil {
		logger.Error("Failed to fetch base totalcmd list", "err", err)
		baseEntries = []CatalogEntry{}
	} else {
		logger.Info("Fetched totalcmd.net entries", "count", len(baseEntries))
	}

	// 3. Resolve & Overlay Community Plugins
	mergedMap := make(map[string]CatalogEntry)

	// Add base entries first
	for _, entry := range baseEntries {
		mergedMap[entry.ID] = entry
	}

	// Helper for normalized comparison
	normalizeStr := func(s string) string {
		return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(s, "_", ""), "-", ""))
	}

	// Overlay community plugins with high priority
	for _, m := range communityManifests {
		logger.Info("Resolving community plugin", "id", m.ID, "source", m.Source.Type)

		entry := CatalogEntry{
			ID:               m.ID,
			Name:             m.Name,
			Type:             m.Type,
			Category:         m.Category,
			Description:      m.Description,
			Authors:          m.Authors,
			Homepage:         m.Homepage,
			License:          m.License,
			Match:            m.Match,
			Source:           m.Source,
			AvailableSources: make(map[string]ResolvedSource),
		}

		// 1. Resolve Primary Source
		switch m.Source.Type {
		case "github_release":
			resolved, err := resolveGitHubRelease(ctx, m.Source.Repo, m.Source.AssetPattern, ghToken)
			if err != nil {
				logger.Error("Failed resolving GitHub release", "plugin", m.ID, "repo", m.Source.Repo, "err", err)
			} else {
				entry.Resolved = resolved
				entry.AvailableSources["github"] = *resolved
			}

		case "direct_url":
			resolved := ResolvedSource{
				Version:      m.Source.Version,
				DownloadURL:  m.Source.DownloadURL,
				ResolvedFrom: "direct",
			}
			entry.Resolved = &resolved
			entry.AvailableSources["direct"] = resolved

		case "totalcmd_net":
			resolved := ResolvedSource{
				DownloadURL:  fmt.Sprintf("https://totalcmd.net/download.php?id=%s", m.Source.TotalcmdID),
				ResolvedFrom: "totalcmd",
			}
			entry.Resolved = &resolved
			entry.AvailableSources["totalcmd"] = resolved
		}

		// 2. Find and link legacy totalcmd.net entry (if exists) into AvailableSources
		normCommunityID := normalizeStr(m.ID)
		normCommunityName := normalizeStr(m.Name)
		normTotalcmdID := normalizeStr(m.Source.TotalcmdID)

		var keysToDelete []string
		for key, existing := range mergedMap {
			if !strings.HasPrefix(key, "totalcmd_") {
				continue
			}

			rawLegacyID := strings.TrimPrefix(key, "totalcmd_")
			normLegacyID := normalizeStr(rawLegacyID)
			normLegacyName := normalizeStr(existing.Name)

			isDuplicate := false

			// Match by explicit totalcmd_id
			if normTotalcmdID != "" && normTotalcmdID == normLegacyID {
				isDuplicate = true
			}
			// Match by ID / Name
			if normCommunityID == normLegacyID || normCommunityName == normLegacyName || normCommunityID == normLegacyName {
				isDuplicate = true
			}
			// Match by aliases
			for _, alias := range m.Match.Aliases {
				normAlias := normalizeStr(alias)
				if normAlias == normLegacyID || normAlias == normLegacyName {
					isDuplicate = true
					break
				}
			}

			if isDuplicate {
				keysToDelete = append(keysToDelete, key)
				// Link totalcmd source if found
				if existing.Resolved != nil {
					entry.AvailableSources["totalcmd"] = *existing.Resolved
				}
			}
		}

		for _, k := range keysToDelete {
			logger.Info("Purged duplicate legacy totalcmd entry", "purged_key", k, "replaced_by", m.ID)
			delete(mergedMap, k)
		}

		// 3. SMART VERSION SELECTION: Pick the absolute highest version across all available sources!
		for srcName, srcInfo := range entry.AvailableSources {
			if entry.Resolved == nil {
				res := srcInfo
				entry.Resolved = &res
				continue
			}

			// If another source has a strictly newer version, promote it to entry.Resolved!
			if CompareVersions(srcInfo.Version, entry.Resolved.Version) > 0 {
				logger.Info("Promoting higher version source to primary resolved",
					"plugin", m.ID,
					"promoted_source", srcName,
					"new_version", srcInfo.Version,
					"previous_version", entry.Resolved.Version,
				)
				res := srcInfo
				entry.Resolved = &res
			}
		}

		mergedMap[m.ID] = entry
	}

	var finalList []CatalogEntry
	for _, entry := range mergedMap {
		finalList = append(finalList, entry)
	}

	snapshot := CatalogSnapshot{
		Version:     1,
		GeneratedAt: time.Now().UTC(),
		TotalCount:  len(finalList),
		Plugins:     finalList,
	}

	if err := os.MkdirAll(*outDir, 0755); err != nil {
		logger.Error("Failed to create output dir", "err", err)
		os.Exit(1)
	}

	// Save catalog.resolved.json
	resolvedFile := filepath.Join(*outDir, "catalog.resolved.json")
	fResolved, err := os.Create(resolvedFile)
	if err != nil {
		logger.Error("Failed to write resolved json", "err", err)
		os.Exit(1)
	}
	defer fResolved.Close()

	enc := json.NewEncoder(fResolved)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snapshot); err != nil {
		logger.Error("Failed encoding json", "err", err)
		os.Exit(1)
	}

	// Save catalog.min.json
	minFile := filepath.Join(*outDir, "catalog.min.json")
	fMin, err := os.Create(minFile)
	if err != nil {
		logger.Error("Failed to write min json", "err", err)
		os.Exit(1)
	}
	defer fMin.Close()

	if err := json.NewEncoder(fMin).Encode(snapshot); err != nil {
		logger.Error("Failed encoding min json", "err", err)
		os.Exit(1)
	}

	logger.Info("Build complete successfully!",
		"output", resolvedFile,
		"total_plugins", snapshot.TotalCount,
	)
}