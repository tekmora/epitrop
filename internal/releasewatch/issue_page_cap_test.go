package releasewatch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func newIssueTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	client, err := NewClient(ClientOptions{
		GitHubBase: srv.URL,
		LookupEnv: func(k string) (string, bool) {
			if k == "GITHUB_TOKEN" {
				return "test-token", true
			}
			return "", false
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func makeTestIssues(total int, bodyLen int, markerIndex int, marker string) []GitHubIssueResponse {
	issues := make([]GitHubIssueResponse, total)
	padding := strings.Repeat("x", bodyLen)
	for i := 0; i < total && i < 1000; i++ {
		body := padding
		if i == markerIndex {
			half := bodyLen / 2
			if half < len(marker) {
				body = marker + strings.Repeat("x", bodyLen-len(marker))
			} else {
				body = strings.Repeat("x", half) + marker + strings.Repeat("y", bodyLen-half-len(marker))
			}
		}
		issues[i] = GitHubIssueResponse{
			Number: i + 1,
			Title:  fmt.Sprintf("issue %d", i+1),
			Body:   body,
			State:  "open",
		}
	}
	return issues
}

func newIssueTestServer(t *testing.T, issues []GitHubIssueResponse, requestCount *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqNum := atomic.AddInt32(requestCount, 1)
		if reqNum > int32(MaxSearchIssuePages) {
			t.Errorf("request #%d exceeds MaxSearchIssuePages (%d)", reqNum, MaxSearchIssuePages)
		}
		if perPage := r.URL.Query().Get("per_page"); perPage != strconv.Itoa(SearchIssuesPerPage) {
			t.Errorf("request per_page = %q, want %d", perPage, SearchIssuesPerPage)
			http.Error(w, "bad per_page", http.StatusBadRequest)
			return
		}
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil || page < 1 {
			t.Errorf("invalid page query: %v", r.URL.Query().Get("page"))
			http.Error(w, "bad page", http.StatusBadRequest)
			return
		}
		start := (page - 1) * SearchIssuesPerPage
		if start >= len(issues) {
			w.Header().Set("Content-Type", "application/json")
			testWrite(w, "[]")
			return
		}
		end := start + SearchIssuesPerPage
		if end > len(issues) {
			end = len(issues)
		}
		chunk := issues[start:end]
		data, err := json.Marshal(chunk)
		if err != nil {
			t.Errorf("json.Marshal chunk: %v", err)
			http.Error(w, "marshal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		testWrite(w, string(data))
	}))
}

func TestMarkerSearchFindsAMarkerBehindPagesOfLongBodies(t *testing.T) {
	marker := issueMarker("github:owner/upstream@v1.0.0")
	issues := makeTestIssues(120, 60000, 37, marker)
	var requests int32
	srv := newIssueTestServer(t, issues, &requests)
	defer srv.Close()

	client := newIssueTestClient(t, srv)
	found, err := searchExistingIssueMarker(context.Background(), client, "owner/consumer", "", marker)
	if err != nil {
		t.Fatalf("searchExistingIssueMarker returned error: %v", err)
	}
	if !found {
		t.Fatal("searchExistingIssueMarker = false, want true")
	}
	if atomic.LoadInt32(&requests) != 4 {
		t.Fatalf("requests = %d, want 4", requests)
	}
}

// A full page of long bodies is larger than the 1 MiB source cap and still fits the
// issue page cap.
func TestMarkerSearchReadsAPageLargerThanTheSourceCap(t *testing.T) {
	marker := issueMarker("github:owner/upstream@v1.0.0")
	issues := makeTestIssues(SearchIssuesPerPage, 200000, SearchIssuesPerPage-1, marker)
	var requests int32
	srv := newIssueTestServer(t, issues, &requests)
	defer srv.Close()

	client := newIssueTestClient(t, srv)
	found, err := searchExistingIssueMarker(context.Background(), client, "owner/consumer", "", marker)
	if err != nil {
		t.Fatalf("searchExistingIssueMarker: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
}

func TestMarkerSearchStopsAfterItsLastPage(t *testing.T) {
	marker := issueMarker("github:owner/upstream@v1.0.0")
	issues := makeTestIssues(500, 100, -1, "")
	var requests int32
	srv := newIssueTestServer(t, issues, &requests)
	defer srv.Close()

	client := newIssueTestClient(t, srv)
	found, err := searchExistingIssueMarker(context.Background(), client, "owner/consumer", "", marker)
	if err != nil {
		t.Fatalf("searchExistingIssueMarker: %v", err)
	}
	if found {
		t.Fatal("found = true, want false")
	}
	if atomic.LoadInt32(&requests) != 50 {
		t.Fatalf("requests = %d, want exactly 50", requests)
	}
}

func TestMarkerSearchStopsAtTheFirstShortPage(t *testing.T) {
	t.Run("ten_issues_two_requests", func(t *testing.T) {
		testBoundaryRequests(t, 10, 2)
	})
	t.Run("nine_issues_one_request", func(t *testing.T) {
		testBoundaryRequests(t, 9, 1)
	})
}

func testBoundaryRequests(t *testing.T, count int, wantRequests int32) {
	t.Helper()
	marker := issueMarker("github:owner/upstream@v1.0.0")
	issues := makeTestIssues(count, 100, -1, "")
	var requests int32
	srv := newIssueTestServer(t, issues, &requests)
	defer srv.Close()

	client := newIssueTestClient(t, srv)
	found, err := searchExistingIssueMarker(context.Background(), client, "owner/consumer", "", marker)
	if err != nil {
		t.Fatalf("searchExistingIssueMarker: %v", err)
	}
	if found {
		t.Fatal("found = true, want false")
	}
	if atomic.LoadInt32(&requests) != wantRequests {
		t.Fatalf("requests = %d, want %d", requests, wantRequests)
	}
}

func TestMarkerSearchFailsOnAPageOverItsCap(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		largeBody := strings.Repeat("A", maxIssuePageBytes+1024)
		if _, writeErr := fmt.Fprintf(w, `[{"number":1,"title":"large","body":%q,"state":"open"}]`, largeBody); writeErr != nil {
			return
		}
	}))
	defer srv.Close()

	client := newIssueTestClient(t, srv)
	marker := issueMarker("github:owner/upstream@v1.0.0")
	found, err := searchExistingIssueMarker(context.Background(), client, "owner/consumer", "", marker)
	if err == nil {
		t.Fatal("searchExistingIssueMarker = nil error, want over-cap error")
	}
	if found {
		t.Fatal("found = true on over-cap error, want false")
	}
	if !strings.Contains(err.Error(), "search issues in owner/consumer page 1") {
		t.Fatalf("err %q does not wrap search issues page context", err.Error())
	}
}

func TestMarkerSearchAsksForSearchIssuesPerPage(t *testing.T) {
	var observedPerPage string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observedPerPage = r.URL.Query().Get("per_page")
		w.Header().Set("Content-Type", "application/json")
		testWrite(w, "[]")
	}))
	defer srv.Close()

	client := newIssueTestClient(t, srv)
	marker := issueMarker("github:owner/upstream@v1.0.0")
	_, err := searchExistingIssueMarker(context.Background(), client, "owner/consumer", "", marker)
	if err != nil {
		t.Fatalf("searchExistingIssueMarker: %v", err)
	}
	if observedPerPage != "10" {
		t.Fatalf("observed per_page = %q, want %q", observedPerPage, "10")
	}
	if observedPerPage != strconv.Itoa(SearchIssuesPerPage) {
		t.Fatalf("observed per_page %q != SearchIssuesPerPage %d", observedPerPage, SearchIssuesPerPage)
	}
}
