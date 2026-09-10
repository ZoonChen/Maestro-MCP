// Package jira implements the W4.5 J3 Maestro↔Jira connector: the
// outbound Jira Server/DC REST client, the SoR→Jira mirror worker and
// the reconcile worker. The sync semantics are frozen by
// SOLUTION-BLUEPRINT section 1.2 and hold structurally here:
//
//   - the client exposes NO transition endpoint and NO status field
//     on the write path; Maestro state reaches Jira only as a
//     `maestro:<status>` LABEL (rule 1: the Jira issue status field
//     and the Maestro work-item status are each untouchable from the
//     other side);
//   - provider unavailability fails closed as ErrJiraUnavailable —
//     reconciliation degrades, the task flow is never blocked.
package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Outbound Jira Server/DC REST client (v2 API, PAT bearer auth — the
// same scheme mcp-atlassian pins for Server/DC deployments). The
// egress hardening mirrors the GitLab client discipline: HTTPS names
// only (no userinfo, no IP literals), redirects refused (a cross-host
// hop is an SSRF vector), TLS verification never disabled, bounded
// body reads, and no writes beyond summary/assignee/labels.

// ErrJiraUnavailable reports transport failures (outage, TLS,
// timeout, provider 5xx): callers degrade — the mirror marks the
// anchor failed, reconciliation skips the cycle — and never fabricate
// provider state.
var ErrJiraUnavailable = errors.New("jira provider unavailable")

// ErrJiraAuthFailed reports a rejected credential (401/403): the PAT
// is wrong or expired; escalating retries cannot fix it.
var ErrJiraAuthFailed = errors.New("jira credential rejected")

// ErrJiraIssueNotFound reports a missing issue (404) — an anchored
// issue deleted on the Jira side is a reconciliation fact, not a
// transport failure.
var ErrJiraIssueNotFound = errors.New("jira issue not found")

// Client is pinned to one Jira instance host.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient builds a client pinned to one approved instance host.
func NewClient(baseURL, token string) (*Client, error) {
	if err := validateProviderURL(baseURL); err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("jira client: token must not be empty")
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http: &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// validateProviderURL mirrors the GitLab registry's host rules at
// egress: the stored base_url was validated at configuration, but
// egress re-checks so a tampered value cannot widen the blast radius.
func validateProviderURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("jira client: base_url is unparseable: %w", err)
	}
	if parsed.Scheme != "https" {
		return errors.New("jira client: base_url must be https")
	}
	if parsed.User != nil {
		return errors.New("jira client: base_url must not carry userinfo")
	}
	host := parsed.Hostname()
	if host == "" || net.ParseIP(host) != nil {
		return errors.New("jira client: host must be a name, not an IP literal")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return errors.New("jira client: base_url must not carry a path")
	}
	return nil
}

// WithTestTransport swaps the egress transport. It exists for the
// package's stub-provider tests (loopback HTTP); production callers
// construct through NewClient and keep the hardened defaults.
func (c *Client) WithTestTransport(transport http.RoundTripper) *Client {
	c.http.Transport = transport
	return c
}

// RemoteIssue is the provider-side issue fact the reconcile worker
// snapshots (read-only channel). Status is display-only: it feeds no
// Maestro state.
type RemoteIssue struct {
	Key      string
	Title    string
	Assignee string // display name; empty when unassigned
	Labels   []string
	Status   string
}

type remoteIssueFields struct {
	Summary  *string       `json:"summary"`
	Assignee *remoteUser   `json:"assignee"`
	Labels   []string      `json:"labels"`
	Status   *remoteStatus `json:"status"`
}

type remoteUser struct {
	Name string `json:"displayName"`
}

type remoteStatus struct {
	Name string `json:"name"`
}

type remoteIssueBody struct {
	Key    string            `json:"key"`
	Fields remoteIssueFields `json:"fields"`
}

// Issue fetches one issue's mirror-relevant fields.
func (c *Client) Issue(ctx context.Context, issueKey string) (*RemoteIssue, error) {
	endpoint := fmt.Sprintf("%s/rest/api/2/issue/%s?fields=summary,assignee,labels,status",
		c.baseURL, url.PathEscape(issueKey))
	var decoded remoteIssueBody
	if err := c.do(ctx, http.MethodGet, endpoint, nil, &decoded); err != nil {
		return nil, err
	}
	return remoteIssueOf(decoded), nil
}

// Search runs one JQL query over the mirror-relevant fields. It is
// the reconcile worker's batch pull (issuekey IN (...) per page).
func (c *Client) Search(ctx context.Context, jql string, maxResults int) ([]RemoteIssue, error) {
	if maxResults <= 0 || maxResults > 100 {
		maxResults = 50
	}
	payload, err := json.Marshal(map[string]any{
		"jql":        jql,
		"maxResults": maxResults,
		"fields":     []string{"summary", "assignee", "labels", "status"},
	})
	if err != nil {
		return nil, fmt.Errorf("jira client: encode search: %w", err)
	}
	var decoded struct {
		Issues []remoteIssueBody `json:"issues"`
	}
	if err := c.do(ctx, http.MethodPost, c.baseURL+"/rest/api/2/search", payload, &decoded); err != nil {
		return nil, err
	}
	issues := make([]RemoteIssue, 0, len(decoded.Issues))
	for _, issue := range decoded.Issues {
		issues = append(issues, *remoteIssueOf(issue))
	}
	return issues, nil
}

// MirrorFieldUpdate is one SoR→Jira field push. Exactly the three
// mirror channels exist; there is no status transition path.
type MirrorFieldUpdate struct {
	Summary  *string
	Assignee *string // empty string assigns nobody (Jira null)
	Labels   []string
}

// Fields reports whether anything would be sent.
func (u MirrorFieldUpdate) IsEmpty() bool {
	return u.Summary == nil && u.Assignee == nil && u.Labels == nil
}

type mirrorUpdateBody struct {
	Fields mirrorUpdateFields `json:"fields"`
}

type mirrorUpdateFields struct {
	Summary  *string     `json:"summary,omitempty"`
	Assignee *mirrorUser `json:"assignee,omitempty"`
	Labels   []string    `json:"labels,omitempty"`
}

// mirrorUser marshals the assignee channel: a name to set, or null to
// unassign. Jira Server resolves users by name on the issue update
// path (accountId is the Cloud-only key).
type mirrorUser struct {
	Name *string `json:"name"`
}

// UpdateMirrorFields pushes mirror values to one issue. The body is
// structurally limited to summary/assignee/labels — the Jira status
// field and transitions endpoint are unreachable through this client.
func (c *Client) UpdateMirrorFields(ctx context.Context, issueKey string, update MirrorFieldUpdate) error {
	if update.IsEmpty() {
		return errors.New("jira client: empty mirror update")
	}
	fields := mirrorUpdateFields{Summary: update.Summary, Labels: update.Labels}
	if update.Assignee != nil {
		assignee := *update.Assignee
		if assignee == "" {
			fields.Assignee = &mirrorUser{Name: nil}
		} else {
			fields.Assignee = &mirrorUser{Name: &assignee}
		}
	}
	payload, err := json.Marshal(mirrorUpdateBody{Fields: fields})
	if err != nil {
		return fmt.Errorf("jira client: encode update: %w", err)
	}
	endpoint := fmt.Sprintf("%s/rest/api/2/issue/%s", c.baseURL, url.PathEscape(issueKey))
	return c.do(ctx, http.MethodPut, endpoint, payload, nil)
}

func remoteIssueOf(body remoteIssueBody) *RemoteIssue {
	issue := &RemoteIssue{Key: body.Key, Labels: body.Fields.Labels}
	if body.Fields.Summary != nil {
		issue.Title = *body.Fields.Summary
	}
	if body.Fields.Assignee != nil {
		issue.Assignee = body.Fields.Assignee.Name
	}
	if body.Fields.Status != nil {
		issue.Status = body.Fields.Status.Name
	}
	return issue
}

// do executes one request with the PAT bearer header and classifies
// failures: transport errors and 5xx answer ErrJiraUnavailable
// (fail-closed degradation), 401/403 ErrJiraAuthFailed, 404
// ErrJiraIssueNotFound. Bodies are bounded to 1 MiB.
func (c *Client) do(ctx context.Context, method, endpoint string, payload []byte, decode any) error {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("jira client: request build: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrJiraUnavailable, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrJiraUnavailable, err)
	}
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
	case response.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrJiraIssueNotFound, endpoint)
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: provider answered %d", ErrJiraAuthFailed, response.StatusCode)
	default:
		return fmt.Errorf("%w: provider answered %d", ErrJiraUnavailable, response.StatusCode)
	}
	if decode == nil {
		return nil
	}
	if err := json.Unmarshal(body, decode); err != nil {
		return fmt.Errorf("jira client: provider payload: %w", err)
	}
	return nil
}
