package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"streamy/internal/version"
)

// GitHubRelease represents a GitHub release.
type GitHubRelease struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	Body        string `json:"body"`
	PublishedAt string `json:"published_at"`
	HTMLURL     string `json:"html_url"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	Assets      []struct {
		Name        string `json:"name"`
		BrowserURL  string `json:"browser_download_url"`
		Size        int64  `json:"size"`
		ContentType string `json:"content_type"`
	} `json:"assets"`
}

// Checker checks for updates from GitHub releases.
type Checker struct {
	repo       string
	httpClient *http.Client
}

// NewChecker creates a new update checker for the given GitHub repository.
func NewChecker(repo string) *Checker {
	return &Checker{
		repo: repo,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// CheckResult holds the result of an update check.
type CheckResult struct {
	UpdateAvailable bool
	CurrentVersion  string
	LatestVersion   string
	ReleaseNotes    string
	DownloadURL     string
	ReleaseURL      string
	PublishedAt     string
}

// Check checks for updates from GitHub releases.
func (c *Checker) Check(ctx context.Context) (CheckResult, error) {
	current := normalizeVersion(version.Version)
	if current == "" {
		return CheckResult{}, fmt.Errorf("current version is empty")
	}

	// Use /releases instead of /releases/latest to include pre-releases
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases", c.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return CheckResult{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "Streamy-UpdateChecker")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return CheckResult{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return CheckResult{}, fmt.Errorf("repository not found or no releases")
	}
	if resp.StatusCode != http.StatusOK {
		return CheckResult{}, fmt.Errorf("GitHub API error: %s", resp.Status)
	}

	var releases []GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return CheckResult{}, fmt.Errorf("decode response: %w", err)
	}

	// Find the latest non-draft release (including pre-releases)
	var latestRelease *GitHubRelease
	for _, r := range releases {
		if !r.Draft {
			latestRelease = &r
			break
		}
	}

	if latestRelease == nil {
		return CheckResult{}, fmt.Errorf("no non-draft releases found")
	}

	latest := normalizeVersion(latestRelease.TagName)
	if latest == "" {
		return CheckResult{}, fmt.Errorf("latest version is empty")
	}

	updateAvailable := compareVersions(current, latest) < 0

	// Find the Windows installer asset
	downloadURL := findWindowsInstaller(latestRelease.Assets)
	if downloadURL == "" {
		downloadURL = latestRelease.HTMLURL // fallback to release page
	}

	return CheckResult{
		UpdateAvailable: updateAvailable,
		CurrentVersion:  version.Version,
		LatestVersion:   latestRelease.TagName,
		ReleaseNotes:    latestRelease.Body,
		DownloadURL:     downloadURL,
		ReleaseURL:      latestRelease.HTMLURL,
		PublishedAt:     latestRelease.PublishedAt,
	}, nil
}

// normalizeVersion removes 'v' prefix and normalizes version string.
func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	return v
}

// compareVersions compares two semantic version strings.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
func compareVersions(a, b string) int {
	// Split into parts (major.minor.patch[-prerelease])
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")

	maxLen := len(aParts)
	if len(bParts) > maxLen {
		maxLen = len(bParts)
	}

	for i := 0; i < maxLen; i++ {
		var aVal, bVal int
		if i < len(aParts) {
			// Remove any pre-release suffix
			clean := strings.Split(aParts[i], "-")[0]
			aVal, _ = strconv.Atoi(clean)
		}
		if i < len(bParts) {
			clean := strings.Split(bParts[i], "-")[0]
			bVal, _ = strconv.Atoi(clean)
		}
		if aVal < bVal {
			return -1
		}
		if aVal > bVal {
			return 1
		}
	}
	return 0
}

// findWindowsInstaller finds the Windows installer asset in the release.
func findWindowsInstaller(assets []struct {
	Name        string `json:"name"`
	BrowserURL  string `json:"browser_download_url"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}) string {
	// Prefer .exe installer, then .msi, then any .exe
	for _, asset := range assets {
		name := strings.ToLower(asset.Name)
		if strings.HasSuffix(name, "-setup.exe") || strings.HasSuffix(name, "-installer.exe") {
			return asset.BrowserURL
		}
	}
	for _, asset := range assets {
		name := strings.ToLower(asset.Name)
		if strings.HasSuffix(name, ".msi") {
			return asset.BrowserURL
		}
	}
	for _, asset := range assets {
		name := strings.ToLower(asset.Name)
		if strings.HasSuffix(name, ".exe") && runtime.GOOS == "windows" {
			return asset.BrowserURL
		}
	}
	return ""
}