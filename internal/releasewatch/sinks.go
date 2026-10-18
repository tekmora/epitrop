package releasewatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	urlPkg "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tekmora/epitrop/internal/config"
)

type searchStatusError struct {
	repo   string
	status int
}

func (e *searchStatusError) Error() string {
	return fmt.Sprintf("search issues in %s returned HTTP %d", e.repo, e.status)
}

func wrapMissingTokenError(err error, repo string, token string) error {
	var statusErr *searchStatusError
	if errors.As(err, &statusErr) && token == "" {
		switch statusErr.status {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return fmt.Errorf("%w: search in %s returned HTTP %d without a token", ErrMissingToken, repo, statusErr.status)
		}
	}
	return err
}

func DeliverGitHubIssue(ctx context.Context, client *Client, state *State, sink *config.GitHubIssueSink, item ReleaseItem) (bool, error) {
	marker := issueMarker(item.ID)
	firstLabel := ""
	if len(sink.Labels) > 0 {
		firstLabel = sink.Labels[0]
	}
	found, err := searchExistingIssueMarker(ctx, client, sink.Repo, firstLabel, marker)
	if err != nil {
		return false, wrapMissingTokenError(err, sink.Repo, client.GitHubToken())
	}
	if found {
		state.MarkSeen(item.ID)
		if saveErr := state.Save(); saveErr != nil {
			return false, saveErr
		}
		client.Logf("release-watch: already filed %s in %s\n", item.ID, sink.Repo)
		return false, nil
	}

	if client.GitHubToken() == "" {
		return false, ErrMissingToken
	}

	body := marker + "\n\n" + item.URL
	if item.Notes != "" {
		body += "\n\n" + fenceNotes(truncateRunes(item.Notes, MaxNotesRunes))
	}
	title := fmt.Sprintf("Upstream release: %s %s", item.SourceName, item.TagOrModel)
	num, err := createGitHubIssue(ctx, client, sink.Repo, GitHubIssuePayload{
		Title:  title,
		Body:   body,
		Labels: sink.Labels,
	})
	if err != nil {
		return false, err
	}

	state.MarkSeen(item.ID)
	if err = state.Save(); err != nil {
		return false, err
	}
	client.Logf("release-watch: filed %s#%d for %s\n", sink.Repo, num, item.ID)
	return true, nil
}

// maxIssuePageBytes bounds one page of the marker search: SearchIssuesPerPage issues of
// at most about 400 KiB each.
const maxIssuePageBytes = 4 * 1024 * 1024

func searchExistingIssueMarker(ctx context.Context, client *Client, repo string, firstLabel string, marker string) (bool, error) {
	for page := 1; page <= MaxSearchIssuePages; page++ {
		issues, err := fetchIssuePage(ctx, client, repo, firstLabel, page)
		if err != nil {
			return false, err
		}
		for i := 0; i < len(issues); i++ {
			if strings.Contains(issues[i].Body, marker) {
				return true, nil
			}
		}
		if len(issues) < SearchIssuesPerPage {
			break
		}
	}
	return false, nil
}

// fetchIssuePage reads one page of up to SearchIssuesPerPage issues of the repository,
// open and closed, filtered by the label when one is given.
func fetchIssuePage(ctx context.Context, client *Client, repo string, firstLabel string, page int) ([]GitHubIssueResponse, error) {
	url := fmt.Sprintf("%s/repos/%s/issues?state=all&per_page=%d&page=%d", client.GitHubBaseURL(), repo, SearchIssuesPerPage, page)
	if firstLabel != "" {
		url += "&labels=" + urlPkg.QueryEscape(firstLabel)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build search issues request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	status, _, body, err := client.DoRequest(ctx, req, 30*time.Second, maxIssuePageBytes)
	if err != nil {
		return nil, fmt.Errorf("search issues in %s page %d: %w", repo, page, err)
	}
	if status != http.StatusOK {
		return nil, &searchStatusError{repo: repo, status: status}
	}
	var issues []GitHubIssueResponse
	if err = json.Unmarshal(body, &issues); err != nil {
		return nil, fmt.Errorf("parse issues response for %s: %w", repo, err)
	}
	return issues, nil
}

func createGitHubIssue(ctx context.Context, client *Client, repo string, payload GitHubIssuePayload) (int, error) {
	bodyData, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal issue payload: %w", err)
	}
	url := fmt.Sprintf("%s/repos/%s/issues", client.GitHubBaseURL(), repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyData))
	if err != nil {
		return 0, fmt.Errorf("build create issue request for %s: %w", repo, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	status, _, body, err := client.DoRequest(ctx, req, 30*time.Second, maxSourceResponseBytes)
	if err != nil {
		return 0, fmt.Errorf("create issue in %s: %w", repo, err)
	}
	if status != http.StatusCreated {
		return 0, fmt.Errorf("create issue in %s returned HTTP %d", repo, status)
	}
	var resp GitHubIssueResponse
	if err = json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("parse create issue response for %s: %w", repo, err)
	}
	return resp.Number, nil
}

func DeliverNtfy(ctx context.Context, client *Client, state *State, sink *config.NtfySink, item ReleaseItem) (bool, error) {
	url := fmt.Sprintf("%s/%s", client.NtfyBaseURL(), sink.Topic)
	title := fmt.Sprintf("Upstream release: %s %s", item.SourceName, item.TagOrModel)
	content := title + "\n\n" + item.URL
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(content))
	if err != nil {
		return false, fmt.Errorf("build ntfy request: %w", err)
	}
	priority := sink.Priority
	if priority <= 0 {
		priority = 3
	}
	req.Header.Set("Priority", strconv.Itoa(priority))

	status, _, _, err := client.DoRequest(ctx, req, 30*time.Second, maxSourceResponseBytes)
	if err != nil {
		return false, fmt.Errorf("ntfy post to %s: %w", sink.Topic, err)
	}
	if status < 200 || status >= 300 {
		return false, fmt.Errorf("ntfy post to %s returned HTTP %d", sink.Topic, status)
	}

	state.MarkSeen(item.ID)
	if err = state.Save(); err != nil {
		return false, err
	}
	return true, nil
}

// issueMarker is the dedupe marker filed into and searched for in issue bodies. The id
// carries upstream tag names, so "<" and ">" are encoded: a tag holding "-->" must not end
// the HTML comment and spill the rest of the marker into the rendered issue.
func issueMarker(id string) string {
	safe := strings.NewReplacer("<", "%3C", ">", "%3E").Replace(id)
	return "<!-- release-watch: " + safe + " -->"
}

// fenceNotes puts upstream release notes in a code block. GitHub turns @names, #numbers and
// links to other repositories in issue text into notifications and cross-references on
// the upstream project; inside a code block it does neither. The fence is longer than
// any backtick run in the notes, so the notes cannot close it.
func fenceNotes(notes string) string {
	longest, run := 0, 0
	for _, r := range notes {
		if r == '`' {
			run++
			longest = max(longest, run)
			continue
		}
		run = 0
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "text\n" + notes + "\n" + fence
}

func truncateRunes(text string, maxRunes int) string {
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes])
}
