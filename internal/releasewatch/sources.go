package releasewatch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tekmora/epitrop/internal/config"
)

const maxSourceResponseBytes = 1024 * 1024

func FetchSource(ctx context.Context, client *Client, src config.ReleaseWatchSource) ([]ReleaseItem, error) {
	if src.GitHub != "" {
		return fetchGitHubSource(ctx, client, src.GitHub)
	}
	if src.HuggingFaceOrg != "" {
		return fetchHuggingFaceSource(ctx, client, src.HuggingFaceOrg)
	}
	return nil, fmt.Errorf("source specifies neither github nor huggingface_org")
}

func fetchGitHubSource(ctx context.Context, client *Client, repo string) ([]ReleaseItem, error) {
	url := fmt.Sprintf("%s/repos/%s/releases?per_page=10", client.GitHubBaseURL(), repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build github releases request for %s: %w", repo, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	status, _, body, err := client.DoRequest(ctx, req, 30*time.Second, maxSourceResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("fetch github releases for %s: %w", repo, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("github releases for %s returned HTTP %d", repo, status)
	}

	var rawReleases []GitHubReleasePayload
	if err = json.Unmarshal(body, &rawReleases); err != nil {
		return nil, fmt.Errorf("parse github releases JSON for %s: %w", repo, err)
	}
	if len(rawReleases) == 0 {
		return fetchGitHubTags(ctx, client, repo)
	}

	items := make([]ReleaseItem, 0, len(rawReleases))
	for i := 0; i < len(rawReleases); i++ {
		rel := rawReleases[i]
		if rel.Draft || rel.Prerelease {
			continue
		}
		htmlURL := rel.HTMLURL
		if htmlURL == "" {
			htmlURL = fmt.Sprintf("https://github.com/%s/releases/tag/%s", repo, rel.TagName)
		}
		items = append(items, ReleaseItem{
			ID:          SeenPrefix(config.ReleaseWatchSource{GitHub: repo}) + rel.TagName,
			SourceType:  "github",
			SourceName:  repo,
			TagOrModel:  rel.TagName,
			URL:         htmlURL,
			Notes:       rel.Body,
			PublishedAt: rel.PublishedAt,
		})
	}
	return items, nil
}

func fetchGitHubTags(ctx context.Context, client *Client, repo string) ([]ReleaseItem, error) {
	url := fmt.Sprintf("%s/repos/%s/tags?per_page=10", client.GitHubBaseURL(), repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build github tags request for %s: %w", repo, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	status, _, body, err := client.DoRequest(ctx, req, 30*time.Second, maxSourceResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("fetch github tags for %s: %w", repo, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("github tags for %s returned HTTP %d", repo, status)
	}

	var rawTags []GitHubTagPayload
	if err = json.Unmarshal(body, &rawTags); err != nil {
		return nil, fmt.Errorf("parse github tags JSON for %s: %w", repo, err)
	}

	items := make([]ReleaseItem, 0, len(rawTags))
	for i := 0; i < len(rawTags); i++ {
		tag := rawTags[i]
		items = append(items, ReleaseItem{
			ID:          SeenPrefix(config.ReleaseWatchSource{GitHub: repo}) + tag.Name,
			SourceType:  "github",
			SourceName:  repo,
			TagOrModel:  tag.Name,
			URL:         fmt.Sprintf("https://github.com/%s/releases/tag/%s", repo, tag.Name),
			Notes:       "",
			PublishedAt: time.Now().UTC(),
		})
	}
	return items, nil
}

func fetchHuggingFaceSource(ctx context.Context, client *Client, org string) ([]ReleaseItem, error) {
	url := fmt.Sprintf("%s/api/models?author=%s&sort=createdAt&direction=-1&limit=20", client.HuggingFaceBaseURL(), org)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build huggingface models request for %s: %w", org, err)
	}

	status, _, body, err := client.DoRequest(ctx, req, 30*time.Second, maxSourceResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("fetch huggingface models for %s: %w", org, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("huggingface models for %s returned HTTP %d", org, status)
	}

	var rawModels []HuggingFaceModelPayload
	if err = json.Unmarshal(body, &rawModels); err != nil {
		return nil, fmt.Errorf("parse huggingface models JSON for %s: %w", org, err)
	}

	items := make([]ReleaseItem, 0, len(rawModels))
	for i := 0; i < len(rawModels); i++ {
		m := rawModels[i]
		modelName := m.ID
		if !strings.Contains(modelName, "/") {
			modelName = org + "/" + modelName
		}
		items = append(items, ReleaseItem{
			ID:          "hf:" + modelName,
			SourceType:  "huggingface",
			SourceName:  org,
			TagOrModel:  m.ID,
			URL:         fmt.Sprintf("https://huggingface.co/%s", modelName),
			Notes:       "",
			PublishedAt: m.CreatedAt,
		})
	}
	return items, nil
}
