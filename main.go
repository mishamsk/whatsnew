package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const unavailable = "changelog is not available"

type repoRef struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}

func (r repoRef) String() string {
	if r.Owner == "" || r.Repo == "" {
		return ""
	}
	return r.Owner + "/" + r.Repo
}

type cacheEntry struct {
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	ResolvedAt string `json:"resolved_at"`
	Source     string `json:"source"`
}

type repoCache struct {
	path   string
	Values map[string]cacheEntry `json:"values"`
}

type item struct {
	Source           string
	Kind             string
	Name             string
	InstalledVersion string
	CurrentVersion   string
	TargetVersion    string
	TagHint          string
	CacheKey         string
	Repo             repoRef
	RepoSource       string
	NotesTitle       string
	NotesBody        string
	NotesSource      string
}

func (i item) title() string {
	label := i.Name
	if i.Kind != "" {
		label = i.Kind + "/" + label
	}
	return fmt.Sprintf("%s %s", i.Source, label)
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cache, err := loadCache()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cache disabled: %v\n", err)
		cache = &repoCache{Values: map[string]cacheEntry{}}
	}

	var items []item
	if commandExists("brew") {
		brewItems, err := collectBrew(ctx, cache)
		if err != nil {
			fmt.Fprintf(os.Stderr, "brew skipped: %v\n", err)
		} else {
			items = append(items, brewItems...)
		}
	}
	if commandExists("mise") {
		miseItems, err := collectMise(ctx, cache)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mise skipped: %v\n", err)
		} else {
			items = append(items, miseItems...)
		}
	}
	if len(items) == 0 {
		fmt.Println("No Homebrew or mise tools found.")
		return
	}

	gh := githubClient{
		useGH: commandExists("gh"),
		token: strings.TrimSpace(os.Getenv("GITHUB_TOKEN")),
		http:  &http.Client{Timeout: 20 * time.Second},
	}

	for idx := range items {
		fmt.Fprintf(os.Stderr, "Fetching %d/%d: %s\n", idx+1, len(items), items[idx].title())
		fillNotes(ctx, gh, &items[idx])
	}
	if err := cache.save(); err != nil {
		fmt.Fprintf(os.Stderr, "cache save failed: %v\n", err)
	}

	p := tea.NewProgram(newModel(items), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tui failed: %v\n", err)
		os.Exit(1)
	}
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func runJSON(ctx context.Context, out any, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("%s %v: %w: %s", name, args, err, msg)
		}
		return fmt.Errorf("%s %v: %w", name, args, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %v returned invalid json: %w", name, args, err)
	}
	return nil
}

func runText(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("%s %v: %w: %s", name, args, err, msg)
		}
		return "", fmt.Errorf("%s %v: %w", name, args, err)
	}
	return string(data), nil
}

func loadCache() (*repoCache, error) {
	base := strings.TrimSpace(os.Getenv("XDG_CACHE_DIR"))
	if base == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		base = dir
	}
	c := &repoCache{
		path:   filepath.Join(base, "whatsnew", "repos.json"),
		Values: map[string]cacheEntry{},
	}
	data, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(data, &c.Values); err != nil {
		var wrapped struct {
			Values map[string]cacheEntry `json:"values"`
		}
		if err2 := json.Unmarshal(data, &wrapped); err2 != nil {
			return nil, err
		}
		c.Values = wrapped.Values
	}
	if c.Values == nil {
		c.Values = map[string]cacheEntry{}
	}
	return c, nil
}

func (c *repoCache) get(key string) (repoRef, bool) {
	if c == nil || key == "" {
		return repoRef{}, false
	}
	entry, ok := c.Values[key]
	if !ok || entry.Owner == "" || entry.Repo == "" {
		return repoRef{}, false
	}
	return repoRef{Owner: entry.Owner, Repo: entry.Repo}, true
}

func (c *repoCache) put(key string, repo repoRef, source string) {
	if c == nil || key == "" || repo.String() == "" {
		return
	}
	c.Values[key] = cacheEntry{
		Owner:      repo.Owner,
		Repo:       repo.Repo,
		ResolvedAt: time.Now().UTC().Format(time.RFC3339),
		Source:     source,
	}
}

func (c *repoCache) save() error {
	if c == nil || c.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c.Values, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, append(data, '\n'), 0o644)
}

type brewOutdated struct {
	Formulae []struct {
		Name              string   `json:"name"`
		InstalledVersions []string `json:"installed_versions"`
		CurrentVersion    string   `json:"current_version"`
	} `json:"formulae"`
	Casks []struct {
		Name              string   `json:"name"`
		InstalledVersions []string `json:"installed_versions"`
		CurrentVersion    string   `json:"current_version"`
	} `json:"casks"`
}

type brewInfo struct {
	Formulae []brewFormula `json:"formulae"`
	Casks    []brewCask    `json:"casks"`
}

type brewFormula struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Tap      string `json:"tap"`
	Homepage string `json:"homepage"`
	Versions struct {
		Stable string `json:"stable"`
	} `json:"versions"`
	URLs map[string]struct {
		URL      string `json:"url"`
		Tag      string `json:"tag"`
		Revision string `json:"revision"`
		Branch   string `json:"branch"`
	} `json:"urls"`
	Installed []struct {
		Version string `json:"version"`
		Time    int64  `json:"time"`
	} `json:"installed"`
	Outdated       bool   `json:"outdated"`
	RubySourcePath string `json:"ruby_source_path"`
}

type brewCask struct {
	Token          string `json:"token"`
	FullToken      string `json:"full_token"`
	Tap            string `json:"tap"`
	Homepage       string `json:"homepage"`
	URL            string `json:"url"`
	Version        string `json:"version"`
	Installed      string `json:"installed"`
	InstalledTime  int64  `json:"installed_time"`
	Outdated       bool   `json:"outdated"`
	RubySourcePath string `json:"ruby_source_path"`
}

func collectBrew(ctx context.Context, cache *repoCache) ([]item, error) {
	var outdated brewOutdated
	if err := runJSON(ctx, &outdated, "brew", "outdated", "--json=v2"); err != nil {
		return nil, err
	}

	var items []item
	for _, f := range outdated.Formulae {
		info, err := brewInfoFor(ctx, false, f.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "brew %s skipped: %v\n", f.Name, err)
			continue
		}
		if len(info.Formulae) == 0 {
			continue
		}
		it := item{
			Source:           "brew",
			Kind:             "formula",
			Name:             f.Name,
			InstalledVersion: strings.Join(f.InstalledVersions, ", "),
			CurrentVersion:   f.CurrentVersion,
			TargetVersion:    f.CurrentVersion,
			CacheKey:         "brew:formula:" + f.Name,
		}
		resolveBrewFormula(ctx, cache, &it, info.Formulae[0])
		items = append(items, it)
	}
	for _, c := range outdated.Casks {
		info, err := brewInfoFor(ctx, true, c.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "brew cask %s skipped: %v\n", c.Name, err)
			continue
		}
		if len(info.Casks) == 0 {
			continue
		}
		it := item{
			Source:           "brew",
			Kind:             "cask",
			Name:             c.Name,
			InstalledVersion: strings.Join(c.InstalledVersions, ", "),
			CurrentVersion:   c.CurrentVersion,
			TargetVersion:    c.CurrentVersion,
			CacheKey:         "brew:cask:" + c.Name,
		}
		resolveBrewCask(ctx, cache, &it, info.Casks[0])
		items = append(items, it)
	}
	if len(items) > 0 {
		return items, nil
	}

	var installed brewInfo
	if err := runJSON(ctx, &installed, "brew", "info", "--json=v2", "--installed"); err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour).Unix()
	for _, f := range installed.Formulae {
		t, v := newestFormulaInstall(f)
		if t < cutoff {
			continue
		}
		name := f.Name
		if f.FullName != "" {
			name = f.FullName
		}
		it := item{
			Source:           "brew",
			Kind:             "formula",
			Name:             name,
			InstalledVersion: v,
			CurrentVersion:   f.Versions.Stable,
			TargetVersion:    v,
			CacheKey:         "brew:formula:" + name,
		}
		resolveBrewFormula(ctx, cache, &it, f)
		items = append(items, it)
	}
	for _, c := range installed.Casks {
		if c.InstalledTime < cutoff {
			continue
		}
		name := c.Token
		if c.FullToken != "" {
			name = c.FullToken
		}
		it := item{
			Source:           "brew",
			Kind:             "cask",
			Name:             name,
			InstalledVersion: c.Installed,
			CurrentVersion:   c.Version,
			TargetVersion:    c.Installed,
			CacheKey:         "brew:cask:" + name,
		}
		resolveBrewCask(ctx, cache, &it, c)
		items = append(items, it)
	}
	return items, nil
}

func brewInfoFor(ctx context.Context, cask bool, name string) (brewInfo, error) {
	var info brewInfo
	args := []string{"info", "--json=v2"}
	if cask {
		args = append(args, "--cask")
	}
	args = append(args, name)
	err := runJSON(ctx, &info, "brew", args...)
	return info, err
}

func newestFormulaInstall(f brewFormula) (int64, string) {
	var newest int64
	var version string
	for _, installed := range f.Installed {
		if installed.Time >= newest {
			newest = installed.Time
			version = installed.Version
		}
	}
	return newest, version
}

func resolveBrewFormula(ctx context.Context, cache *repoCache, it *item, f brewFormula) {
	if repo, ok := cache.get(it.CacheKey); ok {
		it.Repo = repo
		it.RepoSource = "cache"
		return
	}
	candidates := []string{}
	if stable, ok := f.URLs["stable"]; ok {
		candidates = append(candidates, stable.URL)
		if stable.Tag != "" {
			it.TagHint = stable.Tag
		}
	}
	if head, ok := f.URLs["head"]; ok {
		candidates = append(candidates, head.URL)
	}
	candidates = append(candidates, f.Homepage)
	if repo, tag := firstGitHubRepo(candidates); repo.String() != "" {
		it.Repo = repo
		if it.TagHint == "" {
			it.TagHint = tag
		}
		it.RepoSource = "brew info"
		cache.put(it.CacheKey, repo, it.RepoSource)
		return
	}
	resolveFromRuby(ctx, cache, it, f.Tap, f.RubySourcePath)
}

func resolveBrewCask(ctx context.Context, cache *repoCache, it *item, c brewCask) {
	if repo, ok := cache.get(it.CacheKey); ok {
		it.Repo = repo
		it.RepoSource = "cache"
		return
	}
	if repo, tag := firstGitHubRepo([]string{c.URL, c.Homepage}); repo.String() != "" {
		it.Repo = repo
		it.TagHint = tag
		it.RepoSource = "brew info"
		cache.put(it.CacheKey, repo, it.RepoSource)
		return
	}
	resolveFromRuby(ctx, cache, it, c.Tap, c.RubySourcePath)
}

func resolveFromRuby(ctx context.Context, cache *repoCache, it *item, tap, sourcePath string) {
	sourceURL := rubySourceURL(tap, sourcePath)
	if sourceURL == "" {
		return
	}
	body, err := fetchText(ctx, sourceURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "source fetch failed for %s: %v\n", it.Name, err)
		return
	}
	repo, tag := repoFromRubySource(body)
	if repo.String() == "" {
		return
	}
	it.Repo = repo
	it.TagHint = tag
	it.RepoSource = "ruby source"
	cache.put(it.CacheKey, repo, it.RepoSource)
}

func rubySourceURL(tap, sourcePath string) string {
	if sourcePath == "" {
		return ""
	}
	switch tap {
	case "homebrew/core":
		return "https://raw.githubusercontent.com/Homebrew/homebrew-core/HEAD/" + sourcePath
	case "homebrew/cask":
		return "https://raw.githubusercontent.com/Homebrew/homebrew-cask/HEAD/" + sourcePath
	}
	parts := strings.Split(tap, "/")
	if len(parts) != 2 {
		return ""
	}
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/homebrew-%s/HEAD/%s", parts[0], parts[1], sourcePath)
}

func repoFromRubySource(source string) (repoRef, string) {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`https://github\.com/[^"'\s]+/releases/download/[^"'\s]+`),
		regexp.MustCompile(`https://github\.com/[^"'\s]+/archive/refs/tags/[^"'\s]+`),
		regexp.MustCompile(`https://github\.com/[^"'\s]+\.git`),
		regexp.MustCompile(`https://github\.com/[^"'\s]+`),
	}
	for _, pattern := range patterns {
		for _, match := range pattern.FindAllString(source, -1) {
			if repo, tag := githubRepoFromURL(match); repo.String() != "" {
				return repo, tag
			}
		}
	}
	return repoRef{}, ""
}

func firstGitHubRepo(values []string) (repoRef, string) {
	for _, value := range values {
		if repo, tag := githubRepoFromURL(value); repo.String() != "" {
			return repo, tag
		}
	}
	return repoRef{}, ""
}

func githubRepoFromURL(raw string) (repoRef, string) {
	if raw == "" {
		return repoRef{}, ""
	}
	raw = strings.Trim(raw, `"' `)
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Host, "github.com") {
		return repoRef{}, ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return repoRef{}, ""
	}
	owner := parts[0]
	repo := strings.TrimSuffix(parts[1], ".git")
	if ignoreGitHubRepo(owner, repo) {
		return repoRef{}, ""
	}
	tag := ""
	for idx := 2; idx < len(parts); idx++ {
		if idx+1 < len(parts) && parts[idx] == "download" {
			tag = parts[idx+1]
			break
		}
		if idx+2 < len(parts) && parts[idx] == "refs" && parts[idx+1] == "tags" {
			tag = strings.TrimSuffix(parts[idx+2], ".tar.gz")
			tag = strings.TrimSuffix(tag, ".zip")
			break
		}
	}
	return repoRef{Owner: owner, Repo: repo}, tag
}

func ignoreGitHubRepo(owner, repo string) bool {
	ownerLower := strings.ToLower(owner)
	repoLower := strings.ToLower(repo)
	if ownerLower == "homebrew" {
		return true
	}
	return strings.HasPrefix(repoLower, "homebrew-")
}

func fetchText(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "whatsnew")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return string(data), err
}

type miseListing map[string][]struct {
	Version          string `json:"version"`
	RequestedVersion string `json:"requested_version"`
	Installed        bool   `json:"installed"`
	Active           bool   `json:"active"`
}

type miseRegistry struct {
	Short    string   `json:"short"`
	Backends []string `json:"backends"`
}

func collectMise(ctx context.Context, cache *repoCache) ([]item, error) {
	var listing miseListing
	if err := runJSON(ctx, &listing, "mise", "ls", "--json"); err != nil {
		return nil, err
	}
	var names []string
	for name := range listing {
		names = append(names, name)
	}
	sort.Strings(names)
	var items []item
	for _, name := range names {
		for _, entry := range listing[name] {
			if !entry.Active {
				continue
			}
			target := entry.Version
			it := item{
				Source:           "mise",
				Kind:             "tool",
				Name:             name,
				InstalledVersion: entry.Version,
				CurrentVersion:   entry.RequestedVersion,
				TargetVersion:    target,
				CacheKey:         "mise:tool:" + name,
			}
			resolveMise(ctx, cache, &it, name)
			items = append(items, it)
		}
	}
	return items, nil
}

func resolveMise(ctx context.Context, cache *repoCache, it *item, name string) {
	if repo, ok := cache.get(it.CacheKey); ok {
		it.Repo = repo
		it.RepoSource = "cache"
		return
	}
	if strings.Contains(name, "github.com/") {
		if repo := repoFromMiseID(name); repo.String() != "" {
			it.Repo = repo
			it.RepoSource = "mise id"
			cache.put(it.CacheKey, repo, it.RepoSource)
			return
		}
	}
	var reg miseRegistry
	if err := runJSON(ctx, &reg, "mise", "registry", "--json", name); err != nil {
		return
	}
	for _, backend := range reg.Backends {
		repo := repoFromMiseBackend(backend)
		if repo.String() == "" {
			continue
		}
		it.Repo = repo
		it.RepoSource = "mise registry"
		cache.put(it.CacheKey, repo, it.RepoSource)
		return
	}
}

func repoFromMiseID(id string) repoRef {
	idx := strings.Index(id, "github.com/")
	if idx < 0 {
		return repoRef{}
	}
	rest := id[idx+len("github.com/"):]
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		return repoRef{}
	}
	if ignoreGitHubRepo(parts[0], parts[1]) {
		return repoRef{}
	}
	return repoRef{Owner: parts[0], Repo: parts[1]}
}

func repoFromMiseBackend(backend string) repoRef {
	colon := strings.Index(backend, ":")
	if colon < 0 {
		return repoRef{}
	}
	prefix := backend[:colon]
	rest := backend[colon+1:]
	if prefix != "aqua" && prefix != "ubi" {
		return repoRef{}
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		return repoRef{}
	}
	if ignoreGitHubRepo(parts[0], parts[1]) {
		return repoRef{}
	}
	return repoRef{Owner: parts[0], Repo: parts[1]}
}

type githubClient struct {
	useGH bool
	token string
	http  *http.Client
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
	Body    string `json:"body"`
}

type githubContent struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

func fillNotes(ctx context.Context, gh githubClient, it *item) {
	if it.Repo.String() == "" {
		it.NotesTitle = unavailable
		it.NotesBody = unavailable
		return
	}
	release, ok := fetchRelease(ctx, gh, it.Repo, candidateTags(*it))
	if ok && strings.TrimSpace(release.Body) != "" {
		title := release.Name
		if title == "" {
			title = release.TagName
		}
		it.NotesTitle = title
		it.NotesBody = strings.TrimSpace(release.Body)
		it.NotesSource = "GitHub release " + release.TagName
		return
	}
	if body, ok := fetchChangelog(ctx, gh, it.Repo); ok {
		it.NotesTitle = "CHANGELOG.md"
		it.NotesBody = strings.TrimSpace(body)
		it.NotesSource = "root CHANGELOG.md"
		return
	}
	it.NotesTitle = unavailable
	it.NotesBody = unavailable
}

func candidateTags(it item) []string {
	var tags []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		for _, existing := range tags {
			if existing == v {
				return
			}
		}
		tags = append(tags, v)
	}
	add(it.TagHint)
	add(it.TargetVersion)
	if it.TargetVersion != "" && !strings.HasPrefix(it.TargetVersion, "v") {
		add("v" + it.TargetVersion)
	}
	if strings.Contains(it.TargetVersion, "_") {
		add(strings.Split(it.TargetVersion, "_")[0])
	}
	if strings.Contains(it.TargetVersion, ",") {
		add(strings.Split(it.TargetVersion, ",")[0])
	}
	return tags
}

func fetchRelease(ctx context.Context, gh githubClient, repo repoRef, tags []string) (githubRelease, bool) {
	for _, tag := range tags {
		var release githubRelease
		if err := gh.apiJSON(ctx, fmt.Sprintf("repos/%s/releases/tags/%s", repo.String(), url.PathEscape(tag)), &release); err == nil {
			return release, true
		}
	}
	var latest githubRelease
	if err := gh.apiJSON(ctx, fmt.Sprintf("repos/%s/releases/latest", repo.String()), &latest); err == nil {
		return latest, true
	}
	return githubRelease{}, false
}

func fetchChangelog(ctx context.Context, gh githubClient, repo repoRef) (string, bool) {
	for _, name := range []string{"CHANGELOG.md", "Changelog.md", "changelog.md"} {
		var content githubContent
		if err := gh.apiJSON(ctx, fmt.Sprintf("repos/%s/contents/%s", repo.String(), name), &content); err != nil {
			continue
		}
		if content.Encoding != "base64" {
			continue
		}
		decoded, err := decodeBase64(content.Content)
		if err == nil && strings.TrimSpace(decoded) != "" {
			return decoded, true
		}
	}
	return "", false
}

func (gh githubClient) apiJSON(ctx context.Context, path string, out any) error {
	if gh.useGH {
		if err := gh.apiJSONWithGH(ctx, path, out); err == nil {
			return nil
		}
	}
	return gh.apiJSONHTTP(ctx, path, out)
}

func (gh githubClient) apiJSONWithGH(ctx context.Context, path string, out any) error {
	data, err := runText(ctx, "gh", "api", path)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(data), out)
}

func (gh githubClient) apiJSONHTTP(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "whatsnew")
	if gh.token != "" {
		req.Header.Set("Authorization", "Bearer "+gh.token)
	}
	client := gh.http
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("github api http %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

func decodeBase64(value string) (string, error) {
	cleaned := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			return -1
		}
		return r
	}, value)
	data, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

type model struct {
	items         []item
	selected      int
	scroll        int
	width, height int
}

func newModel(items []item) model {
	return model{items: items}
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.selected > 0 {
				m.selected--
				m.scroll = 0
			}
		case "down", "j":
			if m.selected < len(m.items)-1 {
				m.selected++
				m.scroll = 0
			}
		case "pgup", "b":
			m.scroll -= pageSize(m.height)
			if m.scroll < 0 {
				m.scroll = 0
			}
		case "pgdown", " ", "f":
			m.scroll += pageSize(m.height)
		}
	}
	return m, nil
}

func pageSize(height int) int {
	if height <= 6 {
		return 1
	}
	return height - 6
}

func (m model) View() string {
	if len(m.items) == 0 {
		return "No items.\n"
	}
	width := m.width
	if width <= 0 {
		width = 100
	}
	height := m.height
	if height <= 0 {
		height = 30
	}
	listWidth := min(max(width/3, 30), 48)
	bodyWidth := max(width-listWidth-3, 20)
	bodyHeight := max(height-2, 1)

	left := m.renderList(listWidth, bodyHeight)
	right := m.renderBody(bodyWidth, bodyHeight)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)
}

func (m model) renderList(width, height int) string {
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62"))
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	lines := []string{lipgloss.NewStyle().Bold(true).Render("whatsnew")}
	maxItems := max(height-2, 1)
	start := 0
	if m.selected >= maxItems {
		start = m.selected - maxItems + 1
	}
	for idx := start; idx < len(m.items) && len(lines) < height; idx++ {
		it := m.items[idx]
		version := it.TargetVersion
		if version == "" {
			version = it.CurrentVersion
		}
		line := truncate(fmt.Sprintf("%s %s %s", it.Source, it.Name, version), width)
		if idx == m.selected {
			lines = append(lines, selectedStyle.Width(width).Render(line))
		} else {
			lines = append(lines, muted.Render(line))
		}
	}
	return lipgloss.NewStyle().Width(width).Render(strings.Join(padLines(lines, height), "\n"))
}

func (m model) renderBody(width, height int) string {
	it := m.items[m.selected]
	headerStyle := lipgloss.NewStyle().Bold(true)
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	header := headerStyle.Render(it.title())
	meta := []string{}
	if it.InstalledVersion != "" {
		meta = append(meta, "installed "+it.InstalledVersion)
	}
	if it.CurrentVersion != "" {
		meta = append(meta, "current "+it.CurrentVersion)
	}
	if it.Repo.String() != "" {
		meta = append(meta, "github "+it.Repo.String())
	}
	if it.NotesSource != "" {
		meta = append(meta, it.NotesSource)
	}
	lines := []string{truncate(header, width), truncate(muted.Render(strings.Join(meta, " | ")), width), ""}
	bodyLines := wrapText(it.NotesBody, width)
	available := max(height-len(lines)-1, 1)
	if m.scroll > max(len(bodyLines)-available, 0) {
		m.scroll = max(len(bodyLines)-available, 0)
	}
	end := min(m.scroll+available, len(bodyLines))
	if m.scroll < len(bodyLines) {
		lines = append(lines, bodyLines[m.scroll:end]...)
	}
	footer := muted.Render("up/down select | space/b scroll | q quit")
	lines = append(lines, truncate(footer, width))
	return lipgloss.NewStyle().Width(width).Render(strings.Join(padLines(lines, height), "\n"))
}

func wrapText(text string, width int) []string {
	if width <= 0 {
		return []string{text}
	}
	var lines []string
	for _, raw := range strings.Split(text, "\n") {
		raw = strings.TrimRight(raw, "\r")
		if raw == "" {
			lines = append(lines, "")
			continue
		}
		for len(raw) > width {
			cut := strings.LastIndex(raw[:width], " ")
			if cut <= 0 {
				cut = width
			}
			lines = append(lines, raw[:cut])
			raw = strings.TrimLeft(raw[cut:], " ")
		}
		lines = append(lines, raw)
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

func padLines(lines []string, height int) []string {
	for len(lines) < height {
		lines = append(lines, "")
	}
	if len(lines) > height {
		return lines[:height]
	}
	return lines
}

func truncate(s string, width int) string {
	if width <= 0 || len(s) <= width {
		return s
	}
	if width <= 1 {
		return s[:width]
	}
	if width <= 3 {
		return s[:width]
	}
	return s[:width-3] + "..."
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
