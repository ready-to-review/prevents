package prx

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// cacheRetentionPeriod is how long cache files are kept before cleanup
	cacheRetentionPeriod = 20 * 24 * time.Hour // 20 days
)

// CacheClient wraps the regular Client and adds disk-based caching.
type CacheClient struct {
	*Client
	cacheDir string
}

// cacheEntry represents a cached API response.
type cacheEntry struct {
	Data      json.RawMessage `json:"data"`
	UpdatedAt time.Time       `json:"updated_at"`
	CachedAt  time.Time       `json:"cached_at"`
}

// NewCacheClient creates a new caching client with the given cache directory.
func NewCacheClient(token string, cacheDir string, opts ...Option) (*CacheClient, error) {
	cleanPath := filepath.Clean(cacheDir)
	if !filepath.IsAbs(cleanPath) {
		return nil, fmt.Errorf("cache directory must be absolute path")
	}

	if err := os.MkdirAll(cleanPath, 0700); err != nil {
		return nil, fmt.Errorf("creating cache directory: %w", err)
	}

	client := NewClient(token, opts...)

	cc := &CacheClient{
		Client:   client,
		cacheDir: cleanPath,
	}

	go cc.cleanOldCaches()

	return cc, nil
}


// PullRequest fetches a pull request with all its events and metadata, with caching support.
func (c *CacheClient) PullRequest(ctx context.Context, owner, repo string, prNumber int, referenceTime time.Time) (*PullRequestData, error) {
	c.logger.Info("fetching pull request with cache",
		"owner", owner,
		"repo", repo,
		"pr", prNumber,
		"reference_time", referenceTime,
	)

	pr, err := c.cachedPullRequest(ctx, owner, repo, prNumber, referenceTime)
	if err != nil {
		return nil, fmt.Errorf("fetching pull request: %w", err)
	}

	var events []Event

	pullRequest := PullRequest{
		Number:            pr.Number,
		Title:             pr.Title,
		Body:              pr.Body,
		State:             pr.State,
		Draft:             pr.Draft,
		Merged:            pr.Merged,
		Mergeable:         pr.Mergeable,
		MergeableState:    pr.MergeableState,
		CreatedAt:         pr.CreatedAt,
		UpdatedAt:         pr.UpdatedAt,
		Author:            pr.User.Login,
		AuthorAssociation: pr.AuthorAssociation,
		AuthorBot:         isBot(pr.User),
		Additions:         pr.Additions,
		Deletions:         pr.Deletions,
		ChangedFiles:      pr.ChangedFiles,
	}

	if !pr.ClosedAt.IsZero() {
		pullRequest.ClosedAt = &pr.ClosedAt
	}
	if !pr.MergedAt.IsZero() {
		pullRequest.MergedAt = &pr.MergedAt
	}
	if pr.MergedBy != nil {
		pullRequest.MergedBy = pr.MergedBy.Login
	}

	events = append(events, Event{
		Kind:      PROpened,
		Timestamp: pr.CreatedAt,
		Actor:     pr.User.Login,
		Bot:       isBot(pr.User),
	})

	prUpdatedAt := pr.UpdatedAt

	var errors []error

	// Commits
	commits, err := c.cachedCommits(ctx, owner, repo, prNumber, prUpdatedAt)
	if err != nil {
		c.logger.Error("failed to fetch commits", "error", err)
		errors = append(errors, err)
	} else {
		events = append(events, commits...)
	}

	// Comments
	comments, err := c.cachedComments(ctx, owner, repo, prNumber, prUpdatedAt)
	if err != nil {
		c.logger.Error("failed to fetch comments", "error", err)
		errors = append(errors, err)
	} else {
		events = append(events, comments...)
	}

	// Reviews
	reviews, err := c.cachedReviews(ctx, owner, repo, prNumber, prUpdatedAt)
	if err != nil {
		c.logger.Error("failed to fetch reviews", "error", err)
		errors = append(errors, err)
	} else {
		events = append(events, reviews...)
	}

	// Review comments
	reviewComments, err := c.cachedReviewComments(ctx, owner, repo, prNumber, prUpdatedAt)
	if err != nil {
		c.logger.Error("failed to fetch review comments", "error", err)
		errors = append(errors, err)
	} else {
		events = append(events, reviewComments...)
	}

	// Timeline events
	timelineEvents, err := c.cachedTimelineEvents(ctx, owner, repo, prNumber, prUpdatedAt)
	if err != nil {
		c.logger.Error("failed to fetch timeline events", "error", err)
		errors = append(errors, err)
	} else {
		events = append(events, timelineEvents...)
	}

	// Status checks
	statusChecks, err := c.cachedStatusChecks(ctx, owner, repo, pr, prUpdatedAt)
	if err != nil {
		c.logger.Error("failed to fetch status checks", "error", err)
		errors = append(errors, err)
	} else {
		events = append(events, statusChecks...)
	}

	// Check runs
	checkRuns, err := c.cachedCheckRuns(ctx, owner, repo, pr, prUpdatedAt)
	if err != nil {
		c.logger.Error("failed to fetch check runs", "error", err)
		errors = append(errors, err)
	} else {
		events = append(events, checkRuns...)
	}

	if len(events) == 0 && len(errors) > 0 {
		return nil, fmt.Errorf("failed to fetch any events: %w", errors[0])
	}

	if pr.Merged {
		events = append(events, Event{
			Kind:      PRMerged,
			Timestamp: pr.MergedAt,
			Actor:     pr.MergedBy.Login,
			Bot:       isBot(pr.MergedBy),
		})
	} else if pr.State == "closed" {
		events = append(events, Event{
			Kind:      PRClosed,
			Timestamp: pr.ClosedAt,
			Actor:     pr.User.Login,
			Bot:       isBot(pr.User),
		})
	}

	// Filter events to exclude non-failure status_check events
	events = filterEvents(events)

	sortEventsByTimestamp(events)

	c.logger.Info("successfully fetched pull request with cache",
		"owner", owner,
		"repo", repo,
		"pr", prNumber,
		"event_count", len(events),
		"cache_hits", len(events)-len(errors),
	)

	return &PullRequestData{
		PullRequest: pullRequest,
		Events:      events,
	}, nil
}

// cachedPullRequest fetches a pull request with caching.
func (c *CacheClient) cachedPullRequest(ctx context.Context, owner, repo string, prNumber int, referenceTime time.Time) (*githubPullRequest, error) {
	cacheKey := c.cacheKey("pr", owner, repo, fmt.Sprintf("%d", prNumber))

	var cached cacheEntry
	if c.loadCache(cacheKey, &cached) {
		if cached.CachedAt.After(referenceTime) || cached.CachedAt.Equal(referenceTime) {
			var pr githubPullRequest
			if err := json.Unmarshal(cached.Data, &pr); err != nil {
				c.logger.Warn("failed to unmarshal cached pull request", "error", err)
			} else {
				return &pr, nil
			}
		}
		c.logger.Info("cache miss: pull request expired",
			"owner", owner,
			"repo", repo,
			"pr", prNumber,
			"cached_at", cached.CachedAt,
			"reference_time", referenceTime)
	}

	// Fetch from API
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, prNumber)
	rawData, _, err := c.github.getRaw(ctx, path)
	if err != nil {
		return nil, err
	}

	var pr githubPullRequest
	if err := json.Unmarshal(rawData, &pr); err != nil {
		return nil, fmt.Errorf("unmarshaling pull request: %w", err)
	}

	cached = cacheEntry{
		Data:      rawData,
		CachedAt:  time.Now(),
		UpdatedAt: pr.UpdatedAt,
	}
	if err := c.saveCache(cacheKey, cached); err != nil {
		c.logger.Warn("failed to save pull request to cache", "error", err)
	}

	return &pr, nil
}

// cachedFetch is a generic function for fetching data with caching support.
func (c *CacheClient) cachedFetch(ctx context.Context, dataType, path string, referenceTime time.Time) (json.RawMessage, error) {
	cacheKey := c.cacheKey(dataType, path)

	var cached cacheEntry
	if c.loadCache(cacheKey, &cached) {
		if cached.UpdatedAt.After(referenceTime) || cached.UpdatedAt.Equal(referenceTime) {
			c.logger.Debug("cache hit", "type", dataType, "path", path, "cached_at", cached.CachedAt)
			return cached.Data, nil
		}
		c.logger.Debug("cache miss: "+dataType+" expired", "cached_at", cached.UpdatedAt, "reference_time", referenceTime)
	} else {
		c.logger.Debug("cache miss: "+dataType+" not found")
	}

	// Fetch from API
	rawData, _, err := c.github.getRaw(ctx, path)
	if err != nil {
		return nil, err
	}

	cached = cacheEntry{
		Data:      rawData,
		UpdatedAt: referenceTime,
		CachedAt:  time.Now(),
	}
	if err := c.saveCache(cacheKey, cached); err != nil {
		c.logger.Warn("failed to save to cache", "type", dataType, "error", err)
	}

	return rawData, nil
}


// cachedCommits fetches commits with caching.
func (c *CacheClient) cachedCommits(ctx context.Context, owner, repo string, prNumber int, referenceTime time.Time) ([]Event, error) {
	var allEvents []Event
	page := 1

	for {
		path := fmt.Sprintf("/repos/%s/%s/pulls/%d/commits?page=%d&per_page=%d",
			owner, repo, prNumber, page, maxPerPage)
		
		rawData, err := c.cachedFetch(ctx, "commits", path, referenceTime)
		if err != nil {
			return nil, err
		}

		var commits []*githubPullRequestCommit
		if err := json.Unmarshal(rawData, &commits); err != nil {
			return nil, fmt.Errorf("unmarshaling commits: %w", err)
		}

		// Process commits into events
		for _, commit := range commits {
			event := Event{
				Kind:      Commit,
				Timestamp: commit.Commit.Author.Date,
				Body:      truncate(commit.Commit.Message, 256),
			}
			
			if commit.Author != nil {
				event.Actor = commit.Author.Login
				event.Bot = isBot(commit.Author)
			} else {
				event.Actor = "unknown"
			}
			
			allEvents = append(allEvents, event)
		}

		// Check if there are more pages - if we got less than maxPerPage, we're done
		if len(commits) < maxPerPage {
			break
		}
		page++
	}

	return allEvents, nil
}

// cachedComments fetches comments with caching.
func (c *CacheClient) cachedComments(ctx context.Context, owner, repo string, prNumber int, referenceTime time.Time) ([]Event, error) {
	var allEvents []Event
	page := 1

	for {
		path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?page=%d&per_page=%d",
			owner, repo, prNumber, page, maxPerPage)
		
		rawData, err := c.cachedFetch(ctx, "comments", path, referenceTime)
		if err != nil {
			return nil, err
		}

		var comments []*githubComment
		if err := json.Unmarshal(rawData, &comments); err != nil {
			return nil, fmt.Errorf("unmarshaling comments: %w", err)
		}

		for _, comment := range comments {
			event := createEvent(Comment, comment.CreatedAt, comment.User, comment.Body, comment.AuthorAssociation)
			allEvents = append(allEvents, event)
		}

		if len(comments) < maxPerPage {
			break
		}
		page++
	}

	return allEvents, nil
}

// cachedReviews fetches reviews with caching.
func (c *CacheClient) cachedReviews(ctx context.Context, owner, repo string, prNumber int, referenceTime time.Time) ([]Event, error) {
	var allEvents []Event
	page := 1

	for {
		path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews?page=%d&per_page=%d",
			owner, repo, prNumber, page, maxPerPage)
		
		rawData, err := c.cachedFetch(ctx, "reviews", path, referenceTime)
		if err != nil {
			return nil, err
		}

		var reviews []*githubReview
		if err := json.Unmarshal(rawData, &reviews); err != nil {
			return nil, fmt.Errorf("unmarshaling reviews: %w", err)
		}

		for _, review := range reviews {
			if review.State != "" {
				event := createEvent(Review, review.SubmittedAt, review.User, review.Body, review.AuthorAssociation)
				event.Outcome = review.State // "approved", "changes_requested", "commented"
				allEvents = append(allEvents, event)
			}
		}

		if len(reviews) < maxPerPage {
			break
		}
		page++
	}

	return allEvents, nil
}

// cachedReviewComments fetches review comments with caching.
func (c *CacheClient) cachedReviewComments(ctx context.Context, owner, repo string, prNumber int, referenceTime time.Time) ([]Event, error) {
	var allEvents []Event
	page := 1

	for {
		path := fmt.Sprintf("/repos/%s/%s/pulls/%d/comments?page=%d&per_page=%d",
			owner, repo, prNumber, page, maxPerPage)
		
		rawData, err := c.cachedFetch(ctx, "review_comments", path, referenceTime)
		if err != nil {
			return nil, err
		}

		var comments []*githubReviewComment
		if err := json.Unmarshal(rawData, &comments); err != nil {
			return nil, fmt.Errorf("unmarshaling review comments: %w", err)
		}

		for _, comment := range comments {
			event := createEvent(ReviewComment, comment.CreatedAt, comment.User, comment.Body, comment.AuthorAssociation)
			allEvents = append(allEvents, event)
		}

		if len(comments) < maxPerPage {
			break
		}
		page++
	}

	return allEvents, nil
}

// cachedTimelineEvents fetches timeline events with caching.
func (c *CacheClient) cachedTimelineEvents(ctx context.Context, owner, repo string, prNumber int, referenceTime time.Time) ([]Event, error) {
	var allEvents []Event
	page := 1

	for {
		path := fmt.Sprintf("/repos/%s/%s/issues/%d/timeline?page=%d&per_page=%d",
			owner, repo, prNumber, page, maxPerPage)
		
		rawData, err := c.cachedFetch(ctx, "timeline", path, referenceTime)
		if err != nil {
			return nil, err
		}

		var timelineEvents []*githubTimelineEvent
		if err := json.Unmarshal(rawData, &timelineEvents); err != nil {
			return nil, fmt.Errorf("unmarshaling timeline events: %w", err)
		}

		for _, te := range timelineEvents {
			if te.Actor == nil {
				continue
			}

			var event Event
			switch te.Event {
			case "assigned", "unassigned":
				if te.Assignee == nil {
					continue
				}
				event = Event{
					Kind:      EventKind(te.Event),
					Timestamp: te.CreatedAt,
					Actor:     te.Actor.Login,
					Bot:       isBot(te.Actor),
					Targets:   []string{te.Assignee.Login},
				}
			case "review_requested", "review_request_removed":
				if te.RequestedReviewer == nil {
					continue
				}
				event = Event{
					Kind:      EventKind(te.Event),
					Timestamp: te.CreatedAt,
					Actor:     te.Actor.Login,
					Bot:       isBot(te.Actor),
					Targets:   []string{te.RequestedReviewer.Login},
				}
			case "labeled", "unlabeled":
				if te.Label.Name == "" {
					continue
				}
				event = Event{
					Kind:      EventKind(te.Event),
					Timestamp: te.CreatedAt,
					Actor:     te.Actor.Login,
					Bot:       isBot(te.Actor),
					Body:      te.Label.Name, // Store label name in Body field
				}
			case "mentioned":
				event = Event{
					Kind:      EventKind(te.Event),
					Timestamp: te.CreatedAt,
					Actor:     te.Actor.Login,
					Bot:       isBot(te.Actor),
				}
			case "convert_to_draft", "ready_for_review":
				event = Event{
					Kind:      EventKind(te.Event),
					Timestamp: te.CreatedAt,
					Actor:     te.Actor.Login,
					Bot:       isBot(te.Actor),
				}
			default:
				continue
			}

			allEvents = append(allEvents, event)
		}

		if len(timelineEvents) < maxPerPage {
			break
		}
		page++
	}

	return allEvents, nil
}

// cachedStatusChecks fetches status checks with caching.
func (c *CacheClient) cachedStatusChecks(ctx context.Context, owner, repo string, pr *githubPullRequest, referenceTime time.Time) ([]Event, error) {
	var allEvents []Event
	page := 1

	for {
		path := fmt.Sprintf("/repos/%s/%s/statuses/%s?page=%d&per_page=%d",
			owner, repo, pr.Head.SHA, page, maxPerPage)
		
		rawData, err := c.cachedFetch(ctx, "statuses", path, referenceTime)
		if err != nil {
			return nil, err
		}

		var statuses []*githubStatus
		if err := json.Unmarshal(rawData, &statuses); err != nil {
			return nil, fmt.Errorf("unmarshaling statuses: %w", err)
		}

		for _, status := range statuses {
			event := Event{
				Kind:      StatusCheck,
				Timestamp: status.CreatedAt,
				Actor:     status.Creator.Login,
				Bot:       isBot(status.Creator),
				Body:      status.Context, // Store check name in Body
				Outcome:   status.State,   // Store state in Outcome
			}
			// Include description if available
			if status.Description != "" {
				event.Body = event.Body + ": " + truncate(status.Description, 256)
			}
			allEvents = append(allEvents, event)
		}

		if len(statuses) < maxPerPage {
			break
		}
		page++
	}

	return allEvents, nil
}

// cachedCheckRuns fetches check runs with caching.
func (c *CacheClient) cachedCheckRuns(ctx context.Context, owner, repo string, pr *githubPullRequest, referenceTime time.Time) ([]Event, error) {
	var allEvents []Event
	page := 1

	for {
		path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?page=%d&per_page=%d",
			owner, repo, pr.Head.SHA, page, maxPerPage)
		
		rawData, err := c.cachedFetch(ctx, "check_runs", path, referenceTime)
		if err != nil {
			return nil, err
		}

		var response githubCheckRuns
		if err := json.Unmarshal(rawData, &response); err != nil {
			return nil, fmt.Errorf("unmarshaling check runs: %w", err)
		}

		for _, run := range response.CheckRuns {
			event := Event{
				Kind:      CheckRun,
				Timestamp: run.CompletedAt,
				Actor:     "github",
				Bot:       true,
				Body:      run.Name,       // Store check name in Body
				Outcome:   run.Conclusion, // Store conclusion in Outcome
			}
			if run.CompletedAt.IsZero() {
				event.Timestamp = run.StartedAt
				event.Outcome = run.Status
			}
			allEvents = append(allEvents, event)
		}

		if len(response.CheckRuns) < maxPerPage {
			break
		}
		page++
	}

	return allEvents, nil
}


func (c *CacheClient) cacheKey(parts ...string) string {
	key := strings.Join(parts, "/")
	hash := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", hash)
}

func (c *CacheClient) loadCache(key string, v any) bool {
	path := filepath.Join(c.cacheDir, key+".json")

	file, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			c.logger.Debug("failed to open cache file", "error", err, "path", path)
		}
		return false
	}
	defer file.Close()

	if err := json.NewDecoder(file).Decode(v); err != nil {
		c.logger.Warn("failed to decode cache file", "error", err, "path", path)
		return false
	}

	return true
}

func (c *CacheClient) saveCache(key string, v any) error {
	if len(key) != 64 || !isHexString(key) {
		return fmt.Errorf("invalid cache key format")
	}

	path := filepath.Join(c.cacheDir, key+".json")

	tmpPath := path + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("creating cache file: %w", err)
	}

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(v); err != nil {
		file.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("encoding cache data: %w", err)
	}

	if err := file.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing cache file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming cache file: %w", err)
	}

	return nil
}

func (c *CacheClient) cleanOldCaches() {
	c.logger.Debug("cleaning old cache files")

	entries, err := os.ReadDir(c.cacheDir)
	if err != nil {
		c.logger.Error("failed to read cache directory", "error", err)
		return
	}

	cutoff := time.Now().Add(-cacheRetentionPeriod)
	removed := 0

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			path := filepath.Join(c.cacheDir, entry.Name())
			if err := os.Remove(path); err != nil {
				c.logger.Warn("failed to remove old cache file", "path", path, "error", err)
			} else {
				removed++
			}
		}
	}

	if removed > 0 {
		c.logger.Info("cleaned old cache files", "removed", removed)
	}
}
