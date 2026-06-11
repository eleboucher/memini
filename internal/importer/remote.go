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
	"sync"
	"time"

	"github.com/eleboucher/memini/internal/memory"
)

// errUnauthorized aborts a remote import: a 401/403 will recur for every record.
var errUnauthorized = errors.New("import: remote rejected credentials (401/403)")

const remoteConcurrency = 8

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
	type result struct {
		idx int
		err error
	}

	results := make(chan result, len(recs))
	jobs := make(chan int, len(recs))

	var wg sync.WaitGroup
	for range remoteConcurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				err := c.put(ctx, recs[i], opts)
				results <- result{i, err}
			}
		}()
	}

	for i := range recs {
		jobs <- i
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	var imported int
	var errs []string
	var authErr error
	for r := range results {
		if r.err != nil {
			if errors.Is(r.err, errUnauthorized) {
				authErr = r.err
				continue
			}
			errs = append(errs, fmt.Sprintf("%s: %v", recs[r.idx].ID, r.err))
			continue
		}
		imported++
	}
	return imported, errs, authErr
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
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

// withMeta returns a copy of m with key=val set (never mutating the caller's map).
func withMeta(m map[string]any, key string, val any) map[string]any {
	out := make(map[string]any, len(m)+1)
	maps.Copy(out, m)
	out[key] = val
	return out
}
