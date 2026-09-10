package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The stub sandbox speaks Jira Server REST v2 shapes on loopback; the
// production constructor pins https names, so tests inject the same
// stub-transport discipline as the GitLab client suite.

func sandboxClient(t *testing.T, handler http.Handler) (*Client, *[]string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	requests := &[]string{}
	client, err := NewClient("https://jira.sandbox.example", "sandbox-pat")
	require.NoError(t, err)
	transport := &recordingTransport{server: server, seen: requests}
	return client.WithTestTransport(transport), requests
}

type recordingTransport struct {
	server *httptest.Server
	seen   *[]string
	mu     sync.Mutex
}

func (r *recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	r.mu.Lock()
	*r.seen = append(*r.seen, request.Method+" "+request.URL.Path)
	r.mu.Unlock()
	stubbed := request.Clone(request.Context())
	stubbed.URL.Scheme = "http"
	stubbed.URL.Host = strings.TrimPrefix(r.server.URL, "http://")
	return http.DefaultTransport.RoundTrip(stubbed)
}

func sandboxIssue(key, title, assignee, status string, labels ...string) string {
	assigneeJSON := "null"
	if assignee != "" {
		assigneeJSON = fmt.Sprintf(`{"displayName": %q}`, assignee)
	}
	return fmt.Sprintf(`{"key": %q, "fields": {"summary": %q, "assignee": %s, "status": {"name": %q}, "labels": [%s]}}`,
		key, title, assigneeJSON, status, quoteJoin(labels))
}

func quoteJoin(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, fmt.Sprintf("%q", value))
	}
	return strings.Join(quoted, ",")
}

func TestClientIssueRead(t *testing.T) {
	client, requests := sandboxClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer sandbox-pat", r.Header.Get("Authorization"))
		if r.URL.Path == "/rest/api/2/issue/PEIX-101" {
			fmt.Fprint(w, sandboxIssue("PEIX-101", "支付回调重试修复", "zhang.san", "In Progress", "Sprint-12", "maestro:executing"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))

	issue, err := client.Issue(context.Background(), "PEIX-101")
	require.NoError(t, err)
	assert.Equal(t, "PEIX-101", issue.Key)
	assert.Equal(t, "支付回调重试修复", issue.Title)
	assert.Equal(t, "zhang.san", issue.Assignee)
	assert.Equal(t, "In Progress", issue.Status)
	assert.Equal(t, []string{"Sprint-12", "maestro:executing"}, issue.Labels)
	assert.Equal(t, []string{"GET /rest/api/2/issue/PEIX-101"}, *requests)

	_, err = client.Issue(context.Background(), "PEIX-404")
	assert.ErrorIs(t, err, ErrJiraIssueNotFound)
}

func TestClientSearchJQL(t *testing.T) {
	client, requests := sandboxClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		var payload struct {
			JQL        string   `json:"jql"`
			MaxResults int      `json:"maxResults"`
			Fields     []string `json:"fields"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		assert.Contains(t, payload.JQL, "issuekey IN (PEIX-1, PEIX-2)")
		assert.Equal(t, []string{"summary", "assignee", "labels", "status"}, payload.Fields)
		fmt.Fprintf(w, `{"issues": [%s, %s]}`,
			sandboxIssue("PEIX-1", "one", "", "Open"),
			sandboxIssue("PEIX-2", "two", "li.si", "Done", "maestro:done"))
	}))

	issues, err := client.Search(context.Background(), "issuekey IN (PEIX-1, PEIX-2)", 50)
	require.NoError(t, err)
	require.Len(t, issues, 2)
	assert.Equal(t, "PEIX-1", issues[0].Key)
	assert.Empty(t, issues[0].Assignee)
	assert.Equal(t, "li.si", issues[1].Assignee)
	assert.Equal(t, []string{"maestro:done"}, issues[1].Labels)
	assert.Equal(t, []string{"POST /rest/api/2/search"}, *requests)
}

// The structural no-reverse-write guarantee: the update body carries
// exactly summary/assignee/labels — no status field, and the client
// has no transitions path at all.
func TestClientUpdateMirrorFieldsPayloadShape(t *testing.T) {
	client, _ := sandboxClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPut, r.Method)
		var payload map[string]map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		fields, ok := payload["fields"]
		require.True(t, ok, "issue updates ride the fields envelope")
		assert.Subset(t, []string{"summary", "assignee", "labels"}, mapKeys(fields))
		for key := range fields {
			assert.Contains(t, []string{"summary", "assignee", "labels"}, key,
				"the write surface is structurally limited to mirror fields")
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	title := "新标题"
	assignee := "wang.wu"
	err := client.UpdateMirrorFields(context.Background(), "PEIX-9", MirrorFieldUpdate{
		Summary: &title, Assignee: &assignee, Labels: []string{"maestro:queued", "Sprint-12"},
	})
	require.NoError(t, err)
}

// Unassigning marshals assignee:null (the Jira "no assignee" state)
// rather than an empty name the server would reject.
func TestClientUpdateUnassign(t *testing.T) {
	client, _ := sandboxClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload mirrorUpdateBody
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		require.NotNil(t, payload.Fields.Assignee)
		assert.Nil(t, payload.Fields.Assignee.Name)
		w.WriteHeader(http.StatusNoContent)
	}))
	empty := ""
	require.NoError(t, client.UpdateMirrorFields(context.Background(), "PEIX-9",
		MirrorFieldUpdate{Assignee: &empty}))
}

func mapKeys(mapping map[string]any) []string {
	keys := make([]string, 0, len(mapping))
	for key := range mapping {
		keys = append(keys, key)
	}
	return keys
}

func TestClientFailClosedClassification(t *testing.T) {
	unauthorized, _ := sandboxClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	_, err := unauthorized.Issue(context.Background(), "PEIX-1")
	assert.ErrorIs(t, err, ErrJiraAuthFailed)

	serverError, _ := sandboxClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	_, err = serverError.Search(context.Background(), "project = PEIX", 10)
	assert.ErrorIs(t, err, ErrJiraUnavailable)

	// A dead endpoint classifies as unavailable (the fail-closed
	// degradation the workers rely on).
	dead, err := NewClient("https://jira.dead.example", "pat")
	require.NoError(t, err)
	dead.http.Timeout = 1 // fail fast in-test
	_, err = dead.Issue(context.Background(), "PEIX-1")
	assert.ErrorIs(t, err, ErrJiraUnavailable)
}

func TestClientConstructorDiscipline(t *testing.T) {
	_, err := NewClient("http://jira.example", "pat")
	assert.ErrorContains(t, err, "must be https")
	_, err = NewClient("https://10.0.0.5", "pat")
	assert.ErrorContains(t, err, "not an IP literal")
	_, err = NewClient("https://user:pass@jira.example", "pat")
	assert.ErrorContains(t, err, "userinfo")
	_, err = NewClient("https://jira.example/jira", "pat")
	assert.ErrorContains(t, err, "path")
	_, err = NewClient("https://jira.example", "")
	assert.ErrorContains(t, err, "token must not be empty")
}
