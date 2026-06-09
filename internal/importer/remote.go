package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/eleboucher/memini/internal/memory"
)

// errUnauthorized aborts a remote import: a 401/403 will recur for every record.
var errUnauthorized = errors.New("import: remote rejected credentials (401/403)")

// RemoteClient writes records to a running memini via its REST API (POST
// /v1/memories).
type RemoteClient struct {
	baseURL  string
	token    string
	nsHeader string
	http     *http.Client
}

// NewRemoteClient targets a memini server at baseURL (e.g. https://memini.example.com),
// authenticating with token (optional) and scoping each record via nsHeader.
func NewRemoteClient(baseURL, token, nsHeader string) *RemoteClient {
	if nsHeader == "" {
		nsHeader = "X-Memini-Namespace"
	}
	return &RemoteClient{
		baseURL:  strings.TrimRight(baseURL, "/"),
		token:    token,
		nsHeader: nsHeader,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

// remoteRequest mirrors rest.rememberRequest.
type remoteRequest struct {
	Content    string         `json:"content"`
	Tier       memory.Tier    `json:"tier,omitempty"`
	Summary    string         `json:"summary,omitempty"`
	Tags       []string       `json:"tags,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Importance float64        `json:"importance,omitempty"`
	TTLSeconds *int           `json:"ttl_seconds,omitempty"`
	ID         string         `json:"id,omitempty"`
}

func (c *RemoteClient) write(ctx context.Context, recs []Record, opts Options) (int, []string, error) {
	var imported int
	var errs []string
	for _, r := range recs {
		if err := c.put(ctx, r, opts); err != nil {
			if errors.Is(err, errUnauthorized) {
				return imported, errs, err
			}
			errs = append(errs, fmt.Sprintf("%s: %v", r.ID, err))
			continue
		}
		imported++
	}
	return imported, errs, nil
}

func (c *RemoteClient) put(ctx context.Context, r Record, opts Options) error {
	body := remoteRequest{
		Content:    r.Content,
		Tier:       r.Tier,
		Summary:    r.Summary,
		Tags:       r.Tags,
		Importance: r.Importance,
		ID:         r.ID,
		Metadata:   r.Metadata,
	}
	// The server stamps its own created-at, so preserve the source's in metadata.
	if !r.CreatedAt.IsZero() {
		body.Metadata = withMeta(r.Metadata, "imported_created_at", r.CreatedAt.UTC().Format(time.RFC3339))
	}
	// Approximate absolute expiry as a TTL from now (the API takes ttl_seconds).
	if r.ExpiresAt != nil {
		if secs := int(time.Until(*r.ExpiresAt).Seconds()); secs > 0 {
			body.TTLSeconds = &secs
		}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/memories", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(c.nsHeader, resolveNamespace(r.Namespace, opts.DefaultNamespace))
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return errUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("status %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// withMeta returns a copy of m with key=val set (never mutating the caller's map).
func withMeta(m map[string]any, key string, val any) map[string]any {
	out := make(map[string]any, len(m)+1)
	maps.Copy(out, m)
	out[key] = val
	return out
}
