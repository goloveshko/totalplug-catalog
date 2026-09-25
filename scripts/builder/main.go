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

type MatchRule struct {
	Aliases   []string `json:"aliases,omitempty"`
	Filenames []string `json:"filenames"`
}

type SourceInfo struct {
	Type         string `json:"type"` // "github_release", "direct_url", "totalcmd_net"
	Repo         string `json:"repo,omitempty"`
	AssetPattern string `json:"asset_pattern,omitempty"`
	Version      string `json:"version,omitempty"`
	DownloadURL  string `json:"download_url,omitempty"`
	TotalcmdID   string `json:"totalcmd_id,omitempty"`
}

type PluginManifest struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Type        string     `json:"type"` // "WCX", "WLX", "WFX", "WDX", "UTIL", "LANG"
	Category    string     `json:"category,omitempty"`
	Description string     `json:"description,omitempty"`
	Authors     []string   `json:"authors,omitempty"`
	Homepage    string     `json:"homepage,omitempty"`
	License     string     `json:"license,omitempty"`
	Match       MatchRule  `json:"match"`
	Source      SourceInfo `json:"source"`
}

type ResolvedSource struct {
	Version      string    `json:"version"`
	DownloadURL  string    `json:"download_url"`
	WebURL       string    `json:"web_url,omitempty"`
	Arch         string    `json:"arch,omitempty"`       // "x32", "x64", "x32+x64"
	HasSource    bool      `json:"has_source,omitempty"` // true if open-source or source archive available
	PublishedAt  time.Time `json:"published_at,omitempty"`
	ResolvedFrom string    `json:"resolved_from"` // "github", "direct", "totalcmd"
}

type CatalogEntry struct {
	ID               string                    `json:"id"`
	Name             string                    `json:"name"`
	Type             string                    `json:"type"`
	Category         string                    `json:"category,omitempty"`
	Description      string                    `json:"description,omitempty"`
	Authors          []string                  `json:"authors,omitempty"`
	Homepage         string                    `json:"homepage,omitempty"`
	License          string                    `json:"license,omitempty"`
	Arch             string                    `json:"arch,omitempty"`
	HasSource        bool                      `json:"has_source,omitempty"`
	Match            MatchRule                 `json:"match"`
	Source           SourceInfo                `json:"source"`
	Resolved         *ResolvedSource           `json:"resolved,omitempty"`
	AvailableSources map[string]ResolvedSource `json:"available_sources,omitempty"`
}

type CatalogSnapshot struct {
	Version     int            `json:"version"`
	GeneratedAt time.Time      `json:"generated_at"`
	TotalCount  int            `json:"total_count"`
	Plugins     []CatalogEntry `json:"plugins"`
}

type GitHubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	HTMLURL     string        `json:"html_url"`
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

func parseTotalCmdDate(rawDate string) time.Time {
	rawDate = strings.TrimSpace(rawDate)
	if rawDate == "" {
		return time.Time{}
	}

	layouts := []string{
		"02.01.2006",
		"2.01.2006",
		"2.1.2006",
		"02.1.2006",
		"2006-01-02",
		"02-01-2006",
		"2-1-2006",
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, rawDate); err == nil {
			return t.UTC()
		}
	}

	return time.Time{}
}

func parseTotalCmdArch(rawArch string) (cleanArch string, hasSource bool) {
	raw := strings.ToLower(strings.TrimSpace(rawArch))
	if strings.Contains(raw, "src") || strings.Contains(raw, "source") {
		hasSource = true
		raw = strings.ReplaceAll(raw, "+src", "")
		raw = strings.ReplaceAll(raw, "src", "")
		raw = strings.Trim(raw, "+_ -")
	}

	cleanArch = raw
	if cleanArch == "" {
		cleanArch = "any"
	}
	return cleanArch, hasSource
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
		rawDate := strings.TrimSpace(parts[3])
		category := strings.TrimSpace(parts[4])
		rawArch := strings.TrimSpace(parts[5])

		// The totalcmd.net feed labels categories as "fsplugin", "multiarc",
		// "synplus", etc. — map them to real TC plugin types.
		pType := "UTIL"
		switch strings.ToUpper(category) {
		case "PACKER", "WCX", "MULTIARC":
			pType = "WCX"
		case "LISTER", "WLX", "VIEWER":
			pType = "WLX"
		case "FS", "FSPLUGIN", "WFX", "FILE SYSTEM":
			pType = "WFX"
		case "CONTENT", "WDX", "SYNPLUS":
			pType = "WDX"
		case "LANG", "LANGUAGE":
			pType = "LANG"
		default:
			pType = "UTIL"
		}

		cleanArch, hasSource := parseTotalCmdArch(rawArch)
		pubDate := parseTotalCmdDate(rawDate)
		webURL := fmt.Sprintf("https://totalcmd.net/plugring/%s.html", id)
		downloadURL := fmt.Sprintf("https://totalcmd.net/download.php?id=%s", id)

		resolved := ResolvedSource{
			Version:      ver,
			DownloadURL:  downloadURL,
			WebURL:       webURL,
			Arch:         cleanArch,
			HasSource:    hasSource,
			PublishedAt:  pubDate,
			ResolvedFrom: "totalcmd",
		}

		entry := CatalogEntry{
			ID:        fmt.Sprintf("totalcmd_%s", id),
			Name:      title,
			Type:      pType,
			Category:  category,
			Homepage:  webURL,
			Arch:      cleanArch,
			HasSource: hasSource,
			Match: MatchRule{
				Aliases:   []string{strings.ToLower(title), id},
				Filenames: []string{fmt.Sprintf("%s.%s", strings.ToLower(id), strings.ToLower(pType))},
			},
			Source: SourceInfo{
				Type:        "totalcmd_net",
				TotalcmdID:  id,
				DownloadURL: downloadURL,
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
	cleanRepo := strings.TrimPrefix(repo, "https://github.com/")
	cleanRepo = strings.TrimSuffix(cleanRepo, "/")

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", cleanRepo)
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

	webURL := rel.HTMLURL
	if webURL == "" {
		webURL = fmt.Sprintf("https://github.com/%s", cleanRepo)
	}

	return &ResolvedSource{
		Version:      cleanVer,
		DownloadURL:  downloadURL,
		WebURL:       webURL,
		Arch:         "x32+x64", // GitHub releases for TC almost always provide universal packages
		HasSource:    true,      // GitHub repo is open-source by definition
		PublishedAt:  rel.PublishedAt.UTC(),
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

	communityManifests, err := loadCommunityManifests(*pluginsDir)
	if err != nil {
		logger.Warn("Failed to load community plugins (or folder is empty)", "err", err)
	} else {
		logger.Info("Discovered community manifests", "count", len(communityManifests))
	}

	baseEntries, err := fetchTotalCmdBaseList(ctx)
	if err != nil {
		logger.Error("Failed to fetch base totalcmd list", "err", err)
		baseEntries = []CatalogEntry{}
	} else {
		logger.Info("Fetched totalcmd.net entries", "count", len(baseEntries))
	}

	mergedMap := make(map[string]CatalogEntry)
	for _, entry := range baseEntries {
		mergedMap[entry.ID] = entry
	}

	normalizeStr := func(s string) string {
		return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(s, "_", ""), "-", ""))
	}

	for _, m := range communityManifests {
		logger.Info("Resolving community plugin", "id", m.ID, "source", m.Source.Type)

		homepage := m.Homepage
		if homepage == "" && m.Source.Type == "github_release" {
			homepage = fmt.Sprintf("https://github.com/%s", m.Source.Repo)
		}

		entry := CatalogEntry{
			ID:               m.ID,
			Name:             m.Name,
			Type:             m.Type,
			Category:         m.Category,
			Description:      m.Description,
			Authors:          m.Authors,
			Homepage:         homepage,
			License:          m.License,
			Match:            m.Match,
			Source:           m.Source,
			AvailableSources: make(map[string]ResolvedSource),
		}

		switch m.Source.Type {
		case "github_release":
			resolved, err := resolveGitHubRelease(ctx, m.Source.Repo, m.Source.AssetPattern, ghToken)
			if err != nil {
				logger.Error("Failed resolving GitHub release", "plugin", m.ID, "repo", m.Source.Repo, "err", err)
			} else {
				entry.Resolved = resolved
				entry.Arch = resolved.Arch
				entry.HasSource = resolved.HasSource
				entry.AvailableSources["github"] = *resolved
			}

		case "direct_url":
			resolved := ResolvedSource{
				Version:      m.Source.Version,
				DownloadURL:  m.Source.DownloadURL,
				WebURL:       m.Homepage,
				ResolvedFrom: "direct",
			}
			entry.Resolved = &resolved
			entry.AvailableSources["direct"] = resolved

		case "totalcmd_net":
			resolved := ResolvedSource{
				DownloadURL:  fmt.Sprintf("https://totalcmd.net/download.php?id=%s", m.Source.TotalcmdID),
				WebURL:       fmt.Sprintf("https://totalcmd.net/plugring/%s.html", m.Source.TotalcmdID),
				ResolvedFrom: "totalcmd",
			}
			entry.Resolved = &resolved
			entry.AvailableSources["totalcmd"] = resolved
		}

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

			if normTotalcmdID != "" && normTotalcmdID == normLegacyID {
				isDuplicate = true
			}
			if normCommunityID == normLegacyID || normCommunityName == normLegacyName || normCommunityID == normLegacyName {
				isDuplicate = true
			}
			for _, alias := range m.Match.Aliases {
				normAlias := normalizeStr(alias)
				if normAlias == normLegacyID || normAlias == normLegacyName {
					isDuplicate = true
					break
				}
			}

			if isDuplicate {
				keysToDelete = append(keysToDelete, key)
				if existing.Resolved != nil {
					entry.AvailableSources["totalcmd"] = *existing.Resolved
					if entry.Arch == "" {
						entry.Arch = existing.Arch
					}
				}
			}
		}

		for _, k := range keysToDelete {
			logger.Info("Purged duplicate legacy totalcmd entry", "purged_key", k, "replaced_by", m.ID)
			delete(mergedMap, k)
		}

		for srcName, srcInfo := range entry.AvailableSources {
			if entry.Resolved == nil {
				res := srcInfo
				entry.Resolved = &res
				continue
			}

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

	// Ensure output directory exists
	if err := os.MkdirAll(*outDir, 0755); err != nil {
		logger.Error("Failed to create output dir", "err", err)
		os.Exit(1)
	}

	// 1. Save formatted catalog.resolved.json
	resolvedFile := filepath.Join(*outDir, "catalog.resolved.json")
	if err := saveJSON(resolvedFile, snapshot, true); err != nil {
		logger.Error("Failed to write resolved catalog", "err", err)
		os.Exit(1)
	}

	// 2. Save formatted catalog.json (alias for master catalog)
	masterFile := filepath.Join(*outDir, "catalog.json")
	if err := saveJSON(masterFile, snapshot, true); err != nil {
		logger.Error("Failed to write master catalog", "err", err)
		os.Exit(1)
	}

	// 3. Save compact catalog.min.json (without indentation for smaller payload)
	minFile := filepath.Join(*outDir, "catalog.min.json")
	if err := saveJSON(minFile, snapshot, false); err != nil {
		logger.Error("Failed to write minified catalog", "err", err)
		os.Exit(1)
	}

	logger.Info("Build complete successfully!",
		"output_dir", *outDir,
		"total_plugins", snapshot.TotalCount,
	)
}

// saveJSON encodes any data struct into a JSON file with proper error handling and immediate resource cleanup.
func saveJSON(filePath string, data any, indent bool) error {
	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("creating file %s: %w", filePath, err)
	}
	defer file.Close()

	enc := json.NewEncoder(file)
	if indent {
		enc.SetIndent("", "  ")
	}

	if err := enc.Encode(data); err != nil {
		return fmt.Errorf("encoding JSON to %s: %w", filePath, err)
	}

	return nil
}
