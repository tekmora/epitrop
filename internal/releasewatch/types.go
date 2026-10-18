package releasewatch

import (
	"errors"
	"time"
)

const (
	DefaultGitHubAPIBase      = "https://api.github.com"
	DefaultHuggingFaceAPIBase = "https://huggingface.co"
	DefaultNtfyBase           = "https://ntfy.sh"
	MaxSeenRecords            = 10000
	MaxNotesRunes             = 2000
	// SearchIssuesPerPage is the page size of the marker search. An issue body may hold
	// 65536 characters, which is up to about 400 KiB of JSON once escaped, so a page of
	// 10 stays under maxIssuePageBytes; a page of 100 did not fit the 1 MiB source cap.
	SearchIssuesPerPage = 10
	// MaxSearchIssuePages bounds the marker search to the newest 500 issues
	// (MaxSearchIssuePages * SearchIssuesPerPage).
	MaxSearchIssuePages = 50
	SeenFileName        = "release-watch-seen.json"
)

var (
	ErrThrottled    = errors.New("release-watch: rate limit throttled")
	ErrMissingToken = errors.New("release-watch: GITHUB_TOKEN required to file issue")
)

type ReleaseItem struct {
	ID          string
	SourceType  string
	SourceName  string
	TagOrModel  string
	URL         string
	Notes       string
	PublishedAt time.Time
}

type GitHubReleasePayload struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	HTMLURL     string    `json:"html_url"`
	Body        string    `json:"body"`
	PublishedAt time.Time `json:"published_at"`
}

type GitHubTagPayload struct {
	Name string `json:"name"`
}

type HuggingFaceModelPayload struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
}

type GitHubIssuePayload struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels,omitempty"`
}

type GitHubIssueResponse struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
}
