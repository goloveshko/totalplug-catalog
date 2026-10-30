package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	Source           *SourceInfo               `json:"source,omitempty"`
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

// parseVersionNumbers splits a version into its numbers. A letter ends the
// number it follows rather than continuing it: "0.8b2" is a 0.8 with a b2
// revision, not a 0.82, and reading it as 82 made a 2017 upload look older
// than a 2005 one.
func parseVersionNumbers(v string) []int {
	var parts []int
	var current int
	var hasNum bool

	flush := func() {
		if hasNum {
			parts = append(parts, current)
			current = 0
			hasNum = false
		}
	}

	for _, ch := range v {
		switch {
		case ch >= '0' && ch <= '9':
			current = current*10 + int(ch-'0')
			hasNum = true
		case ch == '.' || ch == '-' || ch == '_':
			flush()
		default:
			flush()
		}
	}
	flush()
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

// feedArch joins the build fields of a feed row. The architectures live in
// two fields and the source flag in a third: "lister|x32|x64|src" is a
// plugin with a 32-bit and a 64-bit build whose source is published too.
// Reading only the first field called the 64-bit build of uLister a 32-bit
// plugin, and the client then refused to serve it to an installed 64-bit
// copy.
func feedArch(parts []string) string {
	values := make([]string, 0, 3)
	for _, i := range []int{5, 6, 7} {
		if i < len(parts) {
			if value := strings.TrimSpace(parts[i]); value != "" {
				values = append(values, value)
			}
		}
	}
	return strings.Join(values, "+")
}

func classifyPluginType(category string) string {
	// The totalcmd.net feed labels categories as "fsplugin", "multiarc",
	// "synplus", etc. — map them to real TC plugin types.
	switch strings.ToUpper(strings.TrimSpace(category)) {
	case "PACKER", "WCX", "MULTIARC":
		return "WCX"
	case "LISTER", "WLX", "VIEWER":
		return "WLX"
	case "FS", "FSPLUGIN", "WFX", "FILE SYSTEM":
		return "WFX"
	case "CONTENT", "WDX", "SYNPLUS":
		return "WDX"
	case "LANG", "LANGUAGE":
		return "LANG"
	default:
		return "UTIL"
	}
}

// versionSuffixRE matches the version an upload's id carries at its end:
// "CVSBrowser_1.0.5", "pbl_tc_utils_1.0.0". A plain number is left alone —
// "deb_64" and "PdbView_2" name other things, not other versions.
var versionSuffixRE = regexp.MustCompile(`_\d+(\.\d+)+$`)

// typePrefixRE matches the plugin type the site puts in front of some ids:
// "wlx_TCSumatraPDF" is the plugin "TCSumatraPDF" filed under the site's own
// namespace, and the prefix is not how the file is named.
var typePrefixRE = regexp.MustCompile(`(?i)^(wlx|wcx|wfx|wdx)_`)

// stripTypePrefix takes the site's namespace off an id.
func stripTypePrefix(id string) string {
	return typePrefixRE.ReplaceAllString(id, "")
}

// splitTitle splits a scraped title into the plugin's name and the
// description the site glued onto it. The site writes the description after
// a spaced dash ("Viewix2 - multi file viewer", "PlugExtDet – Tool to ...")
// and some titles end with a bare dash.
func splitTitle(title string) (name, description string) {
	title = strings.TrimSpace(title)
	for _, sep := range []string{" - ", " – ", " — "} {
		if head, tail, ok := strings.Cut(title, sep); ok {
			return strings.TrimSpace(head), strings.TrimSpace(strings.TrimRight(tail, "- "))
		}
	}
	return strings.TrimSpace(strings.TrimRight(title, "- ")), ""
}

// cleanEntry turns the title the site shows into the name, the description
// and the file names the entry matches on.
//
// The site is not careful with its titles: some are the archive's file name
// ("CVSBrowser_1.0.4.zip"), some are the plugin's own file ("anytag.wdx"),
// and some carry a description glued onto the name. A title that names the
// plugin's file is the one file the entry may be found under — the archive
// it comes in says nothing about the file the plugin installs.
func cleanEntry(id, rawTitle, pluginType string) (name, description string, files []string) {
	name, description = splitTitle(rawTitle)
	ext := "." + strings.ToLower(pluginType)

	fileFromTitle := ""
	switch {
	case strings.HasSuffix(strings.ToLower(name), ext):
		fileFromTitle = strings.ToLower(name)
		name = strings.TrimSpace(name[:len(name)-len(ext)])
	case strings.HasSuffix(strings.ToLower(name), ".zip"):
		// A title that is the archive's name: the plugin's name is that name
		// without the version of the upload.
		base := strings.TrimSpace(name[:len(name)-len(".zip")])
		name = versionSuffixRE.ReplaceAllString(base, "")
		if strings.HasSuffix(strings.ToLower(name), ext) {
			fileFromTitle = strings.ToLower(name)
			name = strings.TrimSpace(name[:len(name)-len(ext)])
		}
	}

	// A title that names the plugin's file is the file itself, and the id
	// guess is dropped: the site prefixes some ids with the plugin type for
	// its own reasons ("wdx_anytag"), and that is not how the file is named.
	if fileFromTitle != "" {
		return strings.TrimSpace(name), description, []string{fileFromTitle}
	}

	// The id carries the plugin's name, sometimes with the version of the
	// upload glued on ("CVSBrowser_1.0.5") and sometimes with the extension
	// already ("mthumbs.wlx").
	base := versionSuffixRE.ReplaceAllString(stripTypePrefix(id), "")
	if strings.HasSuffix(strings.ToLower(base), ext) {
		return strings.TrimSpace(name), description, []string{strings.ToLower(base)}
	}
	return strings.TrimSpace(name), description, []string{strings.ToLower(base) + ext}
}

func fetchTotalCmdBaseList(ctx context.Context, logger *slog.Logger) ([]CatalogEntry, error) {
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

		// The feed carries the builds in two fields and the source in a third.
		rawArch := feedArch(parts)

		pType := classifyPluginType(category)

		cleanArch, hasSource := parseTotalCmdArch(rawArch)
		pubDate := parseTotalCmdDate(rawDate)
		webURL := totalcmdPluginPageURL(id)
		downloadURL := fmt.Sprintf("https://totalcmd.net/download.php?id=%s", id)

		name, description, files := cleanEntry(id, title, pType)

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
			ID:          fmt.Sprintf("totalcmd_%s", id),
			Name:        name,
			Type:        pType,
			Category:    category,
			Description: description,
			Homepage:    webURL,
			Arch:        cleanArch,
			HasSource:   hasSource,
			Match: MatchRule{
				// The id without the site's namespace is the name the ini
				// knows the plugin by: "wlx_TCSumatraPDF" is filed under
				// "TCSumatraPDF" there, and the client looks it up by that.
				Aliases:   dedupeStrings([]string{strings.ToLower(name), id, stripTypePrefix(id)}),
				Filenames: files,
			},
			Source: &SourceInfo{
				Type:        "totalcmd_net",
				TotalcmdID:  id,
				DownloadURL: downloadURL,
			},
			Resolved: &resolved,
		}

		list = append(list, entry)
	}

	// A mid-stream read error or a CP1251 decode failure stops the scanner
	// early; without this check we would publish a silently truncated catalog.
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading totalcmd list stream: %w", err)
	}

	// Two folds: the pages that name the same plugin file, and the uploads of
	// the same plugin. The second one runs last, because the first one leaves
	// the surviving upload with the file names of the pages it replaced.
	return mergeSamePluginEntries(logger, mergeSameFileEntries(logger, list)), nil
}

// mergeSameFileEntries folds the entries that name the same plugin file into
// one: the site keeps a page per upload, and the three pages of CVSBrowser
// are one plugin and not three.
//
// The newest version wins, and a tie goes to the lowest id so the published
// catalog does not depend on the order the feed happens to list things in.
// The aliases and the file names of the folded pages are kept, so a plugin
// filed under an older name is still found.
func mergeSameFileEntries(logger *slog.Logger, entries []CatalogEntry) []CatalogEntry {
	groups := make(map[string][]int)
	for i, e := range entries {
		if len(e.Match.Filenames) == 0 {
			continue
		}
		key := strings.ToLower(e.Match.Filenames[0])
		groups[key] = append(groups[key], i)
	}

	keep := make(map[int]bool, len(entries))
	for _, idxs := range groups {
		if len(idxs) < 2 {
			keep[idxs[0]] = true
			continue
		}

		best := idxs[0]
		for _, i := range idxs[1:] {
			if newerEntry(entries[i], entries[best]) {
				best = i
			}
		}
		keep[best] = true

		for _, i := range idxs {
			if i == best {
				continue
			}
			entries[best].Match.Aliases = dedupeStrings(append(entries[best].Match.Aliases, entries[i].Match.Aliases...))
			entries[best].Match.Filenames = dedupeStrings(append(entries[best].Match.Filenames, entries[i].Match.Filenames...))
			logger.Info("Merged legacy entry into the same plugin file",
				"merged", entries[i].ID, "into", entries[best].ID, "file", entries[best].Match.Filenames[0])
		}
	}

	out := make([]CatalogEntry, 0, len(entries))
	for i, e := range entries {
		if keep[i] {
			out = append(out, e)
		}
	}
	return out
}

// newerEntry reports whether a is the newer of two entries: the higher
// version wins, and a version tie goes to the lower id.
func newerEntry(a, b CatalogEntry) bool {
	av, bv := "", ""
	if a.Resolved != nil {
		av = a.Resolved.Version
	}
	if b.Resolved != nil {
		bv = b.Resolved.Version
	}
	if c := CompareVersions(av, bv); c != 0 {
		return c > 0
	}
	return strings.Compare(a.ID, b.ID) < 0
}

// uploadDate is the day the site published the upload.
func uploadDate(e CatalogEntry) time.Time {
	if e.Resolved == nil {
		return time.Time{}
	}
	return e.Resolved.PublishedAt
}

// newerUpload reports which of two uploads of the same plugin is the current
// one. The site dates every upload, and that date says which page is the
// plugin's current one; the version only breaks a tie, because a plugin's own
// numbering does not always grow with the upload ("1.1d" from 2007 against
// "1.0.3" from 2019).
func newerUpload(a, b CatalogEntry) bool {
	ad, bd := uploadDate(a), uploadDate(b)
	if !ad.IsZero() && !bd.IsZero() && !ad.Equal(bd) {
		return ad.After(bd)
	}
	return newerEntry(a, b)
}

// totalcmdPluginPageURL is the address the site serves a plugin under. The
// redesign moved the pages from "/plugring/<id>.html" to "/plugins/<id>"; the
// old address still answers, but only as a redirect to the new one, so the
// catalog names the page it is served under.
func totalcmdPluginPageURL(id string) string {
	return "https://totalcmd.net/plugins/" + id
}

// pluginNameKey is the name the site shows, reduced to the form two entries of
// the same plugin agree on: lower case with the spacing collapsed.
func pluginNameKey(name string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(name))), " ")
}

// uploadVersionRE matches the number an upload's id ends with: "PEViewer3",
// "TCIrfanViewPlugin_2.0", "wlx_fb2_v2".
var uploadVersionRE = regexp.MustCompile(`(?i)(?:^|[._-])v?\d+(?:\.\d+)*$`)

// samePluginIDs reports whether two site ids name the same plugin: one is the
// other with the number of the upload glued on.
//
// The bit marker is not a number of an upload — "archview64" is the 64-bit
// build of "archview" — so an id that only grows by the marker names another
// plugin, and the client tells the two builds apart by that marker. A word
// there ("beta", "new", "patched") names another channel of the plugin, not
// another upload of it, and the ids stay apart.
func samePluginIDs(a, b string) bool {
	short, long := strings.ToLower(a), strings.ToLower(b)
	if short == long {
		return false
	}
	if len(short) > len(long) {
		short, long = long, short
	}
	if !strings.HasPrefix(long, short) {
		return false
	}
	rest := long[len(short):]
	if rest == "" {
		return false
	}
	if strings.HasSuffix(rest, "64") || strings.HasSuffix(rest, "32") {
		return false
	}
	return uploadVersionRE.MatchString(rest)
}

// mergeSamePluginEntries folds an older upload of a plugin into the newer
// upload of the same plugin.
//
// The site keeps a page per upload, so one plugin can be two entries:
// "peviewer" (PE Viewer 2.0, 2012) and "PEViewer3" (PE Viewer 3.0.18) are one
// plugin in two uploads. The older page is also the one an installed copy
// still matches by file name, so left alone the client answers an installed
// 3.0.18 with the 2.0 entry and points at a page twelve years old.
//
// The evidence is deliberately narrow. The names must agree, and the ids must
// be the same word with a number glued on. A number that is the bit marker
// ("archview" and "archview64") names the two builds of one plugin, and those
// must stay apart. So must two entries that merely sound alike: a language
// pack ("Imagine" and "Русский Imagine") and a plugin pack ("SynWrite" and
// "SynWrite - Binary plugins pack") are not two uploads of one plugin.
func mergeSamePluginEntries(logger *slog.Logger, entries []CatalogEntry) []CatalogEntry {
	// Only the entries that share a name can be the same plugin, and few names
	// are shared at all, so the comparison stays small.
	byName := make(map[string][]int)
	for i, e := range entries {
		key := pluginNameKey(e.Name)
		if key == "" {
			continue
		}
		byName[key] = append(byName[key], i)
	}

	folded := make(map[int]bool)
	for _, idxs := range byName {
		for a := 0; a < len(idxs); a++ {
			for b := a + 1; b < len(idxs); b++ {
				i, j := idxs[a], idxs[b]
				if folded[i] || folded[j] {
					continue
				}
				if !samePluginIDs(entries[i].ID, entries[j].ID) {
					continue
				}
				best, other := i, j
				if newerUpload(entries[j], entries[i]) {
					best, other = j, i
				}
				entries[best].Match.Aliases = dedupeStrings(append(entries[best].Match.Aliases, entries[other].Match.Aliases...))
				entries[best].Match.Filenames = dedupeStrings(append(entries[best].Match.Filenames, entries[other].Match.Filenames...))
				folded[other] = true
				logger.Info("Folded an older upload into the newer one",
					"folded", entries[other].ID,
					"into", entries[best].ID,
				)
			}
		}
	}

	out := make([]CatalogEntry, 0, len(entries))
	for i, e := range entries {
		if !folded[i] {
			out = append(out, e)
		}
	}
	return out
}

// fileClaim is one plugin's claim on one file name: the manifest that made
// it and the source the plugin comes from.
type fileClaim struct {
	path   string
	source string
}

// sourceIdentity names the plugin behind a source, so that two manifests
// pointing at the same plugin are recognized as one plugin however
// differently they are written.
func sourceIdentity(src SourceInfo) string {
	switch src.Type {
	case "github_release":
		return "github:" + strings.ToLower(strings.TrimSpace(src.Repo))
	case "totalcmd_net":
		return "totalcmd:" + strings.ToLower(strings.TrimSpace(src.TotalcmdID))
	case "direct_url":
		return "url:" + strings.TrimSpace(src.DownloadURL)
	}
	return src.Type
}

func loadCommunityManifests(pluginsDir string, logger *slog.Logger) ([]PluginManifest, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	var manifests []PluginManifest
	seenIDs := make(map[string]string)
	seenFiles := make(map[string]fileClaim)

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

		// Base feed entries are keyed "totalcmd_<id>"; a community manifest
		// with that prefix would silently overwrite one of them during merge.
		if strings.HasPrefix(strings.ToLower(m.ID), "totalcmd_") {
			return fmt.Errorf("%s: id %q uses reserved 'totalcmd_' prefix", path, m.ID)
		}
		if prev, dup := seenIDs[m.ID]; dup {
			return fmt.Errorf("duplicate plugin id %q: %s conflicts with %s", m.ID, path, prev)
		}
		seenIDs[m.ID] = path

		// One file may belong to two plugins. Two MDView plugins for Total
		// Commander install "mdview.wlx" each, one on WebView2 and one on the
		// older MSHTML engine, and the catalog has to hold both - the client
		// tells them apart by the version that is installed.
		//
		// What must not happen is the same plugin listed twice under one
		// file: that is one plugin in the catalog twice, and the client would
		// offer the two rows as two candidates for the same file.
		for _, fn := range m.Match.Filenames {
			key := strings.ToLower(strings.TrimSpace(fn))
			if key == "" {
				continue
			}
			claim := fileClaim{path: path, source: sourceIdentity(m.Source)}
			if prev, dup := seenFiles[key]; dup {
				if prev.source == claim.source {
					return fmt.Errorf("%s: file %q is already claimed by %s - one plugin is listed twice", path, fn, prev.path)
				}
				logger.Warn("Two plugins install the same file; the client tells them apart by the installed version",
					"file", fn, "plugin", prev.path, "other", claim.path)
			}
			seenFiles[key] = claim
		}

		// Enforce the plugins/<type>/<id>.json layout: a manifest sitting in a
		// recognized type folder must declare that same type, so a WLX plugin
		// accidentally dropped into plugins/wcx/ fails the build instead of
		// being filed under the wrong category.
		if want, ok := pluginTypeFromDir(path); ok && m.Type != want {
			return fmt.Errorf("%s: type %q does not match its plugins/%s/ directory", path, m.Type, strings.ToLower(want))
		}

		manifests = append(manifests, m)
		return nil
	})

	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return manifests, nil
}

// pluginTypeFromDir reports the plugin type implied by a manifest's parent
// directory (e.g. ".../wlx/foo.json" -> "WLX"). The bool is false for unknown
// directories, so the check only applies to the four recognized type folders.
func pluginTypeFromDir(path string) (string, bool) {
	switch dir := strings.ToUpper(filepath.Base(filepath.Dir(path))); dir {
	case "WCX", "WLX", "WFX", "WDX":
		return dir, true
	default:
		return "", false
	}
}

// detectArchFromAssets infers supported architectures from release asset
// names. TC plugin packages are usually universal, so no arch markers means
// "x32+x64"; explicit markers narrow it to the arches actually published.
func detectArchFromAssets(names []string) string {
	var has32, has64 bool
	for _, name := range names {
		n := strings.ToLower(name)
		// Prevent "x86_64" from matching the x86 (32-bit) marker.
		n = strings.ReplaceAll(n, "x86_64", "amd64")
		n = strings.ReplaceAll(n, "x86-64", "amd64")

		if strings.Contains(n, "win32") || strings.Contains(n, "x32") ||
			strings.Contains(n, "i386") || strings.Contains(n, "i686") ||
			strings.Contains(n, "x86") {
			has32 = true
		}
		if strings.Contains(n, "win64") || strings.Contains(n, "x64") ||
			strings.Contains(n, "amd64") {
			has64 = true
		}
	}

	switch {
	case has32 && !has64:
		return "x32"
	case has64 && !has32:
		return "x64"
	default:
		return "x32+x64"
	}
}

// selectAssetURL picks the download URL from release assets. When pattern is
// non-nil it is authoritative: the first matching asset wins and no fallback is
// applied, so an unmatched pattern yields "" rather than a wrong file. With a
// nil pattern it prefers the first .zip/.rar, falling back to the first asset.
func selectAssetURL(assets []GitHubAsset, pattern *regexp.Regexp) string {
	if pattern != nil {
		for _, asset := range assets {
			if pattern.MatchString(asset.Name) {
				return asset.BrowserDownloadURL
			}
		}
		return ""
	}

	for _, asset := range assets {
		n := strings.ToLower(asset.Name)
		if strings.HasSuffix(n, ".zip") || strings.HasSuffix(n, ".rar") {
			return asset.BrowserDownloadURL
		}
	}
	if len(assets) > 0 {
		return assets[0].BrowserDownloadURL
	}
	return ""
}

func resolveGitHubRelease(ctx context.Context, logger *slog.Logger, repo string, pattern string, ghToken string) (*ResolvedSource, error) {
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
		var err error
		compiledRegexp, err = regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("compiling asset_pattern %q: %w", pattern, err)
		}
	}

	if compiledRegexp != nil {
		// An explicit pattern is authoritative: never fall back to assets[0],
		// which is often "Source code.zip" or a checksum/signature file.
		downloadURL = selectAssetURL(rel.Assets, compiledRegexp)
		if downloadURL == "" {
			logger.Warn("asset_pattern matched no release asset, leaving download_url empty",
				"repo", cleanRepo, "pattern", pattern, "release", rel.TagName)
		}
	} else {
		downloadURL = selectAssetURL(rel.Assets, nil)
	}

	var assetNames []string
	for _, asset := range rel.Assets {
		assetNames = append(assetNames, asset.Name)
	}

	webURL := rel.HTMLURL
	if webURL == "" {
		webURL = fmt.Sprintf("https://github.com/%s", cleanRepo)
	}

	return &ResolvedSource{
		Version:      cleanVer,
		DownloadURL:  downloadURL,
		WebURL:       webURL,
		Arch:         detectArchFromAssets(assetNames),
		HasSource:    true, // public GitHub repo means source is available
		PublishedAt:  rel.PublishedAt.UTC(),
		ResolvedFrom: "github",
	}, nil
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// dropRedundantSources removes available_sources entries that merely echo the
// primary resolved source (same URL and version). The TotalPlug client already
// skips such sources when listing alternatives (checker.go), so pruning them
// at build time cuts roughly a third of the payload without behavior change.
func dropRedundantSources(entry *CatalogEntry) {
	if entry.Resolved == nil {
		return
	}
	for name, src := range entry.AvailableSources {
		if src.DownloadURL == entry.Resolved.DownloadURL &&
			(src.Version == "" || src.Version == entry.Resolved.Version) {
			delete(entry.AvailableSources, name)
		}
	}
	if len(entry.AvailableSources) == 0 {
		entry.AvailableSources = nil
	}
}

func normalizeStr(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(s, "_", ""), "-", ""))
}

// pickPrimarySource selects the resolved source with the highest version among
// the current primary and all available alternatives. Sources are visited in
// sorted-name order so equal versions always resolve to the same winner and
// repeated builds stay byte-stable. It returns the chosen source (which may be
// the unchanged current one) and the name it was promoted from, or "" when the
// current primary already wins.
func pickPrimarySource(current *ResolvedSource, sources map[string]ResolvedSource) (*ResolvedSource, string) {
	best := current
	promotedFrom := ""

	for _, name := range slices.Sorted(maps.Keys(sources)) {
		src := sources[name]
		if best == nil || CompareVersions(src.Version, best.Version) > 0 {
			res := src
			best = &res
			promotedFrom = name
		}
	}

	return best, promotedFrom
}

// pickPreferredLegacy returns the highest-version legacy base-feed entry whose
// id ties back deterministically (lowest id wins on version ties). matches is
// already filtered to entries with a non-nil Resolved source, so the returned
// pointer always carries a usable alternative download.
func pickPreferredLegacy(matches []CatalogEntry) *CatalogEntry {
	if len(matches) == 0 {
		return nil
	}
	best := 0
	for i := range matches {
		a, b := matches[i], matches[best]
		if c := CompareVersions(a.Resolved.Version, b.Resolved.Version); c > 0 ||
			(c == 0 && strings.Compare(a.ID, b.ID) < 0) {
			best = i
		}
	}
	return &matches[best]
}

func main() {
	pluginsDir := flag.String("plugins", "plugins", "Path to community plugins directory")
	outDir := flag.String("out", "dist", "Output directory for compiled catalog")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := context.Background()
	ghToken := os.Getenv("GITHUB_TOKEN")

	logger.Info("Starting TotalPlug catalog build process...")

	communityManifests, err := loadCommunityManifests(*pluginsDir, logger)
	if err != nil {
		// A broken or colliding manifest must fail the build loudly,
		// not disappear silently from the published catalog.
		logger.Error("Failed to load community manifests", "err", err)
		os.Exit(1)
	}
	logger.Info("Discovered community manifests", "count", len(communityManifests))

	baseEntries, err := fetchTotalCmdBaseList(ctx, logger)
	if err != nil {
		// Publishing without the base feed would ship a catalog of only
		// community plugins and wipe ~1200 entries for all clients.
		logger.Error("Failed to fetch base totalcmd list, aborting build", "err", err)
		os.Exit(1)
	}
	if len(baseEntries) == 0 {
		logger.Error("totalcmd.net returned an empty base list, aborting build")
		os.Exit(1)
	}
	logger.Info("Fetched totalcmd.net entries", "count", len(baseEntries))

	mergedMap := make(map[string]CatalogEntry)
	for _, entry := range baseEntries {
		mergedMap[entry.ID] = entry
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
			Source:           &m.Source,
			AvailableSources: make(map[string]ResolvedSource),
		}
		entry.Match.Aliases = dedupeStrings(entry.Match.Aliases)

		switch m.Source.Type {
		case "github_release":
			resolved, err := resolveGitHubRelease(ctx, logger, m.Source.Repo, m.Source.AssetPattern, ghToken)
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
				WebURL:       totalcmdPluginPageURL(m.Source.TotalcmdID),
				ResolvedFrom: "totalcmd",
			}
			entry.Resolved = &resolved
			entry.AvailableSources["totalcmd"] = resolved
		}

		normCommunityID := normalizeStr(m.ID)
		normCommunityName := normalizeStr(m.Name)
		normTotalcmdID := normalizeStr(m.Source.TotalcmdID)

		var keysToDelete []string
		var matchedLegacy []CatalogEntry
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
					matchedLegacy = append(matchedLegacy, existing)
				}
			}
		}

		for _, k := range keysToDelete {
			logger.Info("Purged duplicate legacy totalcmd entry", "purged_key", k, "replaced_by", m.ID)
			delete(mergedMap, k)
		}

		// Several legacy rows can collapse onto one community manifest; pick a
		// stable winner (highest version, then lowest id) so the retained
		// "totalcmd" alternative does not depend on map iteration order.
		if chosen := pickPreferredLegacy(matchedLegacy); chosen != nil {
			entry.AvailableSources["totalcmd"] = *chosen.Resolved
			if entry.Arch == "" {
				entry.Arch = chosen.Arch
			}
		}

		promoted, promotedFrom := pickPrimarySource(entry.Resolved, entry.AvailableSources)
		if promotedFrom != "" {
			logger.Info("Promoting higher version source to primary resolved",
				"plugin", m.ID,
				"promoted_source", promotedFrom,
				"new_version", promoted.Version,
			)
		}
		entry.Resolved = promoted

		dropRedundantSources(&entry)

		mergedMap[m.ID] = entry
	}

	var finalList []CatalogEntry
	for _, entry := range mergedMap {
		finalList = append(finalList, entry)
	}

	// Map iteration order is random; sort so repeated builds produce
	// byte-stable output and small diffs on the Pages artifact.
	slices.SortFunc(finalList, func(a, b CatalogEntry) int {
		return strings.Compare(a.ID, b.ID)
	})

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

	// 1. Save formatted catalog.resolved.json (human-readable snapshot)
	resolvedFile := filepath.Join(*outDir, "catalog.resolved.json")
	if err := saveJSON(resolvedFile, snapshot, true); err != nil {
		logger.Error("Failed to write resolved catalog", "err", err)
		os.Exit(1)
	}

	// 2. Save compact catalog.min.json (client contract: source declaration
	// dropped; see buildMinSnapshot). No indentation for a smaller payload.
	minFile := filepath.Join(*outDir, "catalog.min.json")
	if err := saveJSON(minFile, buildMinSnapshot(snapshot), false); err != nil {
		logger.Error("Failed to write minified catalog", "err", err)
		os.Exit(1)
	}

	logger.Info("Build complete successfully!",
		"output_dir", *outDir,
		"total_plugins", snapshot.TotalCount,
	)
}

// buildMinSnapshot projects the full snapshot into the client-facing
// catalog.min.json by dropping each entry's "source" declaration block. The
// TotalPlug checker consumes only resolved/available_sources/match/arch/
// description, so source is provenance for the human-readable
// catalog.resolved.json alone; omitting it removes the download_url echo and
// trims ~19% of the payload. The input snapshot is left untouched.
func buildMinSnapshot(s CatalogSnapshot) CatalogSnapshot {
	min := s
	min.Plugins = make([]CatalogEntry, len(s.Plugins))
	for i, p := range s.Plugins {
		p.Source = nil
		min.Plugins[i] = p
	}
	return min
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
