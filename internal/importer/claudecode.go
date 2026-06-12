package importer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eleboucher/memini/internal/config"
	"github.com/eleboucher/memini/internal/memory"
)

// Claude Code writes one JSON object per line to
// ~/.claude/projects/<slug>/<session-uuid>.jsonl. We reconstruct verbatim
// user→assistant exchanges from it: each becomes one episodic memory. This is
// the "backfill your history" path — `memini import -source claude-code`.

// maxExchangeBytes bounds each side of an exchange so a giant pasted file or
// tool dump doesn't produce a multi-megabyte memory.
const maxExchangeBytes = 4000

// ccScannerBuffer sizes bufio.Scanner's line buffer. Transcript lines (a whole
// assistant turn with tool calls) routinely exceed the 64KB default.
const ccScannerBuffer = 10 << 20

// ccLine is the subset of a Claude Code transcript record we read.
type ccLine struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	Timestamp   string `json:"timestamp"`
	CWD         string `json:"cwd"`
	SessionID   string `json:"sessionId"`
	GitBranch   string `json:"gitBranch"`
	Message     struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// ccBlock is one content block of an assistant message.
type ccBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// exchange accumulates a single user turn and the assistant text that follows.
type exchange struct {
	user      string
	asst      []string
	ts        string
	cwd       string
	branch    string
	sessionID string
}

// nsResolver memoizes per-directory namespace resolution. A backfill walks many
// transcripts that share a cwd, and resolving each shells out to git; without
// the cache we'd run git once per exchange. resolve() returns the same namespace
// the live plugin writes to (git remote repo name), so backfilled history and
// new memories land together. The env override is intentionally skipped (see
// config.ResolveDirNamespace) so a bulk import across projects doesn't collapse.
type nsResolver struct {
	mu      sync.Mutex
	cache   map[string]string
	demoted map[string]string // cwd -> namespace it fell back to (not the git remote)
	fn      func(string) (string, config.NamespaceSource)
}

func newNSResolver() *nsResolver {
	return &nsResolver{
		cache:   map[string]string{},
		demoted: map[string]string{},
		fn:      config.ResolveDirNamespace,
	}
}

func (r *nsResolver) resolve(cwd string) string {
	if cwd == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ns, ok := r.cache[cwd]; ok {
		return ns
	}
	ns, src := r.fn(cwd)
	r.cache[cwd] = ns
	// The live plugin scopes by the git-remote repo name. If we resolved a
	// transcript's cwd any other way (e.g. git timed out under load, or the cwd
	// no longer exists), the backfill could land in a namespace recall never
	// queries — record it so the caller can warn.
	if src != config.NamespaceFromGitRemote {
		r.demoted[cwd] = ns
	}
	return ns
}

// warnings returns one warning per cwd whose namespace was resolved without the
// git-remote step the live plugin uses, so the operator can spot a backfill that
// may have landed where recall won't look.
func (r *nsResolver) warnings() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.demoted) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.demoted))
	for cwd, ns := range r.demoted {
		out = append(out, fmt.Sprintf("namespace for %q resolved to %q without a git remote; "+
			"if the live plugin uses a different name, run `memini namespace move` after import", cwd, ns))
	}
	sort.Strings(out)
	return out
}

// parseClaudeCode parses one transcript's bytes into per-exchange Records. Used
// by Parse() (stdin / single-file path); session id is derived from the records.
func parseClaudeCode(data []byte) ([]Record, error) {
	return parseClaudeCodeReader(bytes.NewReader(data), "", newNSResolver())
}

// parseClaudeCodeReader reconstructs exchanges from a JSONL stream. fallbackID
// names the session when no record carries a sessionId (e.g. stdin import where
// the filename is the only hint). ns resolves a transcript's cwd to a namespace
// (memoized across the whole backfill so git runs once per project dir).
func parseClaudeCodeReader(r io.Reader, fallbackID string, ns *nsResolver) ([]Record, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), ccScannerBuffer)

	var (
		recs    []Record
		pending *exchange
		emitted int
		lastCWD string
	)
	flush := func() {
		if pending == nil {
			return
		}
		defer func() { pending = nil }()
		// Strip harness chrome (injected reminders, hook output, command
		// wrappers) from each side before storing. A turn that was nothing but
		// noise becomes empty and is skipped.
		user := stripNoise(pending.user)
		asst := stripNoise(strings.Join(pending.asst, "\n"))
		if user == "" || asst == "" {
			return // unanswered turn, thinking/tool-only reply, or pure harness noise — skip
		}
		if isErrorOnlyResponse(asst) {
			return // assistant turn is just a transient API error — no knowledge to store
		}
		sid := firstNonEmpty(pending.sessionID, fallbackID)
		content := "user: " + truncateRunes(user, maxExchangeBytes) +
			"\nassistant: " + truncateRunes(asst, maxExchangeBytes)
		recs = append(recs, Record{
			ID:        fmt.Sprintf("cc:%s:%04d", sid, emitted),
			Namespace: ns.resolve(firstNonEmpty(pending.cwd, lastCWD)),
			Tier:      memory.TierEpisodic,
			Content:   content,
			Tags:      []string{string(SourceClaudeCode)},
			Metadata: map[string]any{
				"session_id": sid,
				"timestamp":  pending.ts,
				"git_branch": pending.branch,
				"source":     string(SourceClaudeCode),
			},
			CreatedAt: parseTime(pending.ts),
			ExpiresAt: episodicExpiry(),
		})
		emitted++
	}

	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var l ccLine
		if json.Unmarshal(line, &l) != nil {
			continue
		}
		if l.CWD != "" {
			lastCWD = l.CWD
		}
		switch l.Type {
		case "user":
			if l.IsSidechain || l.IsMeta {
				continue
			}
			var text string
			if json.Unmarshal(l.Message.Content, &text) != nil {
				continue // array content == tool_result, not a user turn
			}
			if isCommandNoise(text) {
				continue
			}
			flush() // a new user turn closes the previous exchange
			pending = &exchange{
				user: text, ts: l.Timestamp, cwd: l.CWD,
				branch: l.GitBranch, sessionID: l.SessionID,
			}
		case "assistant":
			if pending == nil || l.IsSidechain {
				continue
			}
			var blocks []ccBlock
			if json.Unmarshal(l.Message.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					pending.asst = append(pending.asst, b.Text)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("claude-code: scan: %w", err)
	}
	flush()
	return recs, nil
}

// LoadClaudeCode reads a single transcript .jsonl, or walks a directory (a
// project dir, or ~/.claude/projects) for *.jsonl files. Per-file parse errors
// become warnings rather than aborting the whole walk.
func LoadClaudeCode(path string) (recs []Record, warns []string, err error) {
	return LoadClaudeCodeWithProgress(path, nil)
}

// LoadClaudeCodeWithProgress is like LoadClaudeCode but accepts an optional
// progress callback that fires after each file is parsed.
func LoadClaudeCodeWithProgress(path string, onProgress func(done, total int)) (recs []Record, warns []string, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	// One resolver shared across all files so git runs once per project dir,
	// not once per transcript.
	resolver := newNSResolver()
	if !info.IsDir() {
		recs, warns, err = loadClaudeCodeFile(path, resolver)
		return recs, append(warns, resolver.warnings()...), err
	}

	var files []string
	err = filepath.WalkDir(path, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			warns = append(warns, fmt.Sprintf("%s: %v", p, walkErr))
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		files = append(files, p)
		if onProgress != nil {
			onProgress(0, len(files))
		}
		return nil
	})
	if err != nil {
		return nil, warns, err
	}

	if len(files) == 0 {
		return nil, warns, nil
	}

	type result struct {
		recs  []Record
		warns []string
	}

	workers := runtime.NumCPU()
	if workers > len(files) {
		workers = len(files)
	}

	jobs := make(chan string, len(files))
	results := make(chan result, len(files))

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				fileRecs, fileWarns, ferr := loadClaudeCodeFile(p, resolver)
				if ferr != nil {
					fileWarns = append(fileWarns, fmt.Sprintf("%s: %v", p, ferr))
				}
				results <- result{fileRecs, fileWarns}
			}
		}()
	}

	for _, f := range files {
		jobs <- f
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	var done int
	for r := range results {
		done++
		if onProgress != nil {
			onProgress(done, len(files))
		}
		recs = append(recs, r.recs...)
		warns = append(warns, r.warns...)
	}
	warns = append(warns, resolver.warnings()...)

	return recs, warns, nil
}

// loadClaudeCodeFile parses one transcript file, using its basename (minus
// .jsonl) as the session-id fallback.
func loadClaudeCodeFile(path string, ns *nsResolver) ([]Record, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	fallback := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	recs, err := parseClaudeCodeReader(f, fallback, ns)
	return recs, nil, err
}

// isCommandNoise reports whether a user message is a slash-command wrapper or a
// harness-injected event (local-command output, task notification) rather than a
// real prompt — none of which carry knowledge worth storing as a memory.
func isCommandNoise(s string) bool {
	t := strings.TrimLeft(s, " \t\r\n")
	return strings.HasPrefix(t, "<local-command") ||
		strings.HasPrefix(t, "<command-") ||
		strings.HasPrefix(t, "<task-notification")
}

// isErrorOnlyResponse reports whether an assistant turn is just a transport/API
// error (e.g. "API Error: 500 Internal server error ..."). Such exchanges
// capture a transient failure, not knowledge, and only pollute recall, so the
// importer drops them rather than storing them as episodic memories.
func isErrorOnlyResponse(asst string) bool {
	return strings.HasPrefix(strings.TrimSpace(asst), "API Error:")
}

// episodicExpiry returns now + the episodic TTL, so backfilled history (whose
// CreatedAt is in the past) is not swept as expired-on-arrival.
func episodicExpiry() *time.Time {
	exp := time.Now().UTC().Add(memory.TierEpisodic.DefaultTTL())
	return &exp
}

// truncateRunes cuts s to at most max bytes on a rune boundary, appending a
// marker when truncated.
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Back up to a rune boundary at or before max.
	cut := max
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n[...truncated]"
}

// utf8RuneStart reports whether b is the first byte of a UTF-8 rune.
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
