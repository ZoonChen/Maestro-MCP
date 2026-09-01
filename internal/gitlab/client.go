package gitlab

import (
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

// Outbound GitLab API client (M2-GL-001): read-only pulls for
// reconciliation with the bot token resolved from the instance's
// secret reference. Hardening per GLINT section 7: HTTPS hosts only
// (enforced again here, not just at registration), no userinfo, no IP
// literals, redirects refused (a cross-host hop is an SSRF vector, not
// a convenience), TLS verification never disabled, bounded body reads.
// The client performs NO writes: reconciliation refreshes read models.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// ErrProviderUnavailable reports transport failures (outage, TLS,
// timeout): reconciliation keeps the cached read model and surfaces
// the condition — it never fabricates provider state (GLINT section 5).
var ErrProviderUnavailable = errors.New("gitlab provider unavailable")

// NewClient builds a client pinned to one approved instance host.
func NewClient(baseURL, token string) (*Client, error) {
	if err := validateProviderURL(baseURL); err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("gitlab client: token must not be empty")
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

// validateProviderURL mirrors the registry's host rules at egress: the
// stored base_url was validated at creation, but egress re-checks so a
// tampered row cannot silently widen the blast radius.
func validateProviderURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("gitlab client: base_url is unparseable: %w", err)
	}
	if parsed.Scheme != "https" {
		return errors.New("gitlab client: base_url must be https")
	}
	if parsed.User != nil {
		return errors.New("gitlab client: base_url must not carry userinfo")
	}
	host := parsed.Hostname()
	if host == "" || net.ParseIP(host) != nil {
		return errors.New("gitlab client: host must be a name, not an IP literal")
	}
	return nil
}

// RemoteMergeRequest is the provider-side MR fact reconciliation pulls.
type RemoteMergeRequest struct {
	IID          int64  `json:"iid"`
	State        string `json:"state"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	SourceSHA    string `json:"sha"`
	MergeCommit  string `json:"merge_commit_sha"`
	MergedAt     string `json:"merged_at"`
}

type remoteMRBody struct {
	IID          int64  `json:"iid"`
	State        string `json:"state"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	SHA          string `json:"sha"`
	MergeCommit  string `json:"merge_commit_sha"`
	MergedAt     string `json:"merged_at"`
	DiffRefs     struct {
		BaseSHA string `json:"base_sha"`
		HeadSHA string `json:"head_sha"`
	} `json:"diff_refs"`
	LastCommit struct {
		ID string `json:"id"`
	} `json:"last_commit"`
}

// MergeRequest fetches one merge request from the provider.
func (c *Client) MergeRequest(ctx context.Context, gitlabProjectID, mrIID int64) (*RemoteMergeRequest, error) {
	endpoint := fmt.Sprintf("%s/api/v4/projects/%d/merge_requests/%d", c.baseURL, gitlabProjectID, mrIID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("gitlab client: request build: %w", err)
	}
	request.Header.Set("PRIVATE-TOKEN", c.token)
	request.Header.Set("Accept", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("merge request %d not found on the provider", mrIID)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: provider answered %d", ErrProviderUnavailable, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	var decoded remoteMRBody
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("gitlab client: provider payload: %w", err)
	}
	sourceSHA := decoded.DiffRefs.HeadSHA
	if sourceSHA == "" {
		sourceSHA = decoded.SHA
	}
	if sourceSHA == "" {
		sourceSHA = decoded.LastCommit.ID
	}
	return &RemoteMergeRequest{
		IID:          decoded.IID,
		State:        decoded.State,
		SourceBranch: decoded.SourceBranch,
		TargetBranch: decoded.TargetBranch,
		SourceSHA:    sourceSHA,
		MergeCommit:  decoded.MergeCommit,
		MergedAt:     decoded.MergedAt,
	}, nil
}

// WithTestTransport swaps the egress transport. It exists for the
// package's stub-provider tests (loopback HTTP); production callers
// construct through NewClient and keep the hardened defaults.
func (c *Client) WithTestTransport(transport http.RoundTripper) *Client {
	c.http.Transport = transport
	return c
}
