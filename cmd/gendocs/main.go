// Command gendocs renders docs/reference/configuration.md from the source of
// truth: the env-tagged fields on config.Config, their doc comments, and the
// deprecatedVars table.
//
// The reference used to be a hand-maintained table in README.md. It drifted:
// twelve live knobs went undocumented, two defaults were simply wrong, and the
// twenty-one removed variables (two of which refuse the boot) were recorded
// nowhere at all. Generating the page removes the opportunity for that to
// happen again.
//
// The generator is deliberately strict. It refuses to emit anything when a
// variable is missing from the group table or has no doc comment, so a new
// setting cannot reach a release without also reaching the docs. That is the
// specific failure it exists to prevent: MEMINI_STABILITY_K shipped a changed
// recall default that no document mentioned.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/doc/comment"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// group is one section of the reference. The generator owns this table rather
// than reading a struct tag so that config.go stays untouched; the tradeoff is
// that a new variable must be added here, which is exactly the gate we want.
const (
	noDefault = "none"
	nsSource  = "internal/config/namespace.go"
)

type group struct {
	Key   string
	Title string
	Intro string
	Vars  []string
}

var groups = []group{
	{
		Key:   "server",
		Title: "HTTP server",
		Intro: "Listeners and request lifetime. `MEMINI_METRICS_ADDR` and `MEMINI_UI_ADDR` move `/metrics` and the " +
			"admin UI onto their own ports, which is how you expose the UI to a trusted network without exposing the " +
			"API, or scrape metrics without a bearer token.",
		Vars: []string{
			"MEMINI_HTTP_ADDR",
			"MEMINI_SHUTDOWN_TIMEOUT",
			"MEMINI_REQUEST_TIMEOUT",
			"MEMINI_METRICS_ADDR",
			"MEMINI_UI_ADDR",
			"MEMINI_UI_ENABLED",
		},
	},
	{
		Key:   "logging",
		Title: "Logging",
		Vars:  []string{"MEMINI_LOG_LEVEL", "MEMINI_LOG_FORMAT"},
	},
	{
		Key:   "storage",
		Title: "Storage",
		Intro: "memini runs on an embedded SQLite file by default and needs no configuration to start. Point it at " +
			"Postgres when you outgrow one machine.",
		Vars: []string{"MEMINI_BACKEND", "MEMINI_SQLITE_PATH", "MEMINI_POSTGRES_DSN"},
	},
	{
		Key:   "embeddings",
		Title: "Embeddings",
		Intro: "An OpenAI-compatible `/embeddings` endpoint, which you deploy. This is the one thing memini needs from " +
			"you: without it, recall falls back to keyword-only search.\n\n`MEMINI_EMBED_DIMS` must match the model " +
			"you point at. A mismatch is the single most common setup failure, and it corrupts the store rather than " +
			"erroring cleanly.",
		Vars: []string{
			"MEMINI_EMBED_BASE_URL",
			"MEMINI_EMBED_API_KEY",
			"MEMINI_EMBED_MODEL",
			"MEMINI_EMBED_DIMS",
			"MEMINI_EMBED_QUERY_PREFIX",
			"MEMINI_EMBED_MAX_BATCH",
			"MEMINI_EMBED_MAX_BATCH_CHARS",
			"MEMINI_EMBED_MAX_ITEM_CHARS",
			"MEMINI_EMBED_MAX_CONCURRENCY",
			"MEMINI_CHUNK_EMBED",
			"MEMINI_CHUNK_SIZE",
			"MEMINI_CHUNK_OVERLAP",
			"MEMINI_CHUNK_MIN_CONTENT",
			"MEMINI_CHUNK_MAX_PER_MEMORY",
			"MEMINI_CHUNK_SCORE_WEIGHT",
			"MEMINI_REEMBED_ON_MODEL_CHANGE",
		},
	},
	{
		Key:   "llm",
		Title: "LLM (optional)",
		Intro: "Entirely opt-in. With no LLM configured memini still runs the full memory lifecycle using marker " +
			"heuristics: write-time extraction, tier classification, promotion, corroboration and contradiction " +
			"handling all work. Configuring one adds background consolidation, `POST /v1/answer`, the `memory_answer` " +
			"MCP tool, and `MEMINI_RERANK=llm`.\n\nNote that `memory_answer` is only registered as an MCP tool when " +
			"an LLM is configured. Without one, agents will not see the tool at all.",
		Vars: []string{
			"MEMINI_LLM_BASE_URL",
			"MEMINI_LLM_API_KEY",
			"MEMINI_LLM_MODEL",
			"MEMINI_LLM_API",
			"MEMINI_LLM_MAX_TOKENS",
			"MEMINI_LLM_EXTRA_BODY",
		},
	},
	{
		Key:   "rerank",
		Title: "Reranking",
		Intro: "An optional read-side rerank over the hybrid candidates. It only helps where base recall has headroom: " +
			"on session-level benchmarks hybrid is already near ceiling and reranking is a no-op, while on " +
			"turn-level retrieval it is worth double-digit gains. See [the benchmarks](../../bench/README.md).\n\nA " +
			"cross-encoder is the better default when you need one. It gets most of the LLM's lift at a fraction of " +
			"the latency and needs no chat model.",
		Vars: []string{
			"MEMINI_RERANK",
			"MEMINI_RERANK_MODEL",
			"MEMINI_RERANK_API_KEY",
			"MEMINI_RERANK_POOL",
			"MEMINI_RERANK_MIN_SCORE",
			"MEMINI_RERANK_TIMEOUT",
			"MEMINI_RERANK_MAX_BATCH_CHARS",
			"MEMINI_RERANK_MAX_DOC_CHARS",
			"MEMINI_RERANK_LLM_MAX_DOC_CHARS",
			"MEMINI_RERANK_MAX_CONCURRENCY",
		},
	},
	{
		Key:   "activity",
		Title: "Activity log",
		Intro: "An append-only record of what memory was served and why: every read and write, one row per operation " +
			"and memory, carrying the recall query, the rank and the composite score. It backs `GET /v1/activity` " +
			"and the Activity view in the admin UI, and it is how you answer \"why did recall return that?\" rather " +
			"than guessing.\n\nIt is on by default. Writes are best-effort and happen off the request path, so the " +
			"cost is storage rather than latency, and the retention settings below are what bound it.",
		Vars: []string{"MEMINI_ACTIVITY_LOG", "MEMINI_ACTIVITY_RETENTION", "MEMINI_ACTIVITY_MAX_ROWS"},
	},
	{
		Key:   "recall",
		Title: "Recall tuning",
		Intro: "These shape what recall returns and in what order. If recall quality is your problem, start with [the " +
			"tuning guide](../guides/tuning-recall.md) rather than turning these one at a time.",
		Vars: []string{
			"MEMINI_RECALL_MIN_SCORE",
			"MEMINI_RECALL_MIN_SEMANTIC_SCORE",
			"MEMINI_RECALL_SEMANTIC_RESERVE",
			"MEMINI_RECALL_EMBED_TIMEOUT",
			"MEMINI_RECALL_REWRITE_TIMEOUT",
			"MEMINI_STABILITY_K",
			"MEMINI_ASSESSED_SALIENCE_WEIGHT",
			"MEMINI_TURN_ECHO_WINDOW",
			"MEMINI_CASCADE",
		},
	},
	{
		Key:   "write",
		Title: "Write-time dedup and contradiction",
		Intro: "What happens when a new write closely resembles, or directly contradicts, something already stored.",
		Vars: []string{
			"MEMINI_WRITE_EMBED_TIMEOUT",
			"MEMINI_WRITE_DEDUP_SCORE",
			"MEMINI_WRITE_DEDUP_ACTION",
			"MEMINI_SPLIT_DEDUP_LLM_MERGE",
			"MEMINI_CONTRADICT_DOWNRANK",
			"MEMINI_EPISODIC_MIN_CHARS",
			"MEMINI_CLASSIFY_MAX_CHARS",
		},
	},
	{
		Key:   "consolidation",
		Title: "Consolidation and promotion",
		Intro: "How raw captures become durable knowledge. Runs with an LLM when one is configured and with marker " +
			"heuristics otherwise, so durable facts still accumulate in an embedder-only deployment.",
		Vars: []string{
			"MEMINI_CONSOLIDATE_MODE",
			"MEMINI_CONSOLIDATE_MIN_SCORE",
			"MEMINI_DISTILL_BATCH_TOKENS",
			"MEMINI_DISTILL_BATCH_MAX_AGE",
			"MEMINI_DISTILL_TIMEOUT",
			"MEMINI_PROMOTE_INTERVAL",
			"MEMINI_PROMOTE_MIN_ACCESS",
			"MEMINI_PROMOTE_WHOLE_MAX_CHARS",
		},
	},
	{
		Key:   "repair",
		Title: "Repairing degraded writes",
		Intro: "A write that cannot reach the embedder is still stored — vectorless, keyword-searchable, and marked " +
			"for repair in the same transaction. These settings control how quickly it gets its vector back, along " +
			"with the write-time dedup, corroboration and contradiction routing it had to skip.",
		Vars: []string{
			"MEMINI_REPAIR_POLL_INTERVAL",
			"MEMINI_BACKGROUND_EMBED_TIMEOUT",
			"MEMINI_BACKFILL_INTERVAL",
		},
	},
	{
		Key:   "maintenance",
		Title: "Maintenance and decay",
		Intro: "The background sweeper. Read `MEMINI_DEMOTE_AFTER` carefully: it is on by default, and it moves durable " +
			"memories back to episodic when they are never recalled, unimportant and uncorroborated. " +
			"`MEMINI_TOMBSTONE_TTL` is the only irreversible maintenance action in memini.",
		Vars: []string{
			"MEMINI_SWEEP_INTERVAL",
			"MEMINI_SHORT_TERM_CAP",
			"MEMINI_DEMOTE_AFTER",
			"MEMINI_TOMBSTONE_TTL",
			"MEMINI_DEDUP_INTERVAL",
			"MEMINI_DEDUP_SIMILARITY",
			"MEMINI_DEDUP_TIERS",
			"MEMINI_DEDUP_LLM_MERGE",
		},
	},
	{
		Key:   "auth",
		Title: "Authentication",
		Intro: "See [API keys](../api-keys.md) for the full model, including how a named key's bound home and default " +
			"namespace interact with per-request headers.",
		Vars: []string{"MEMINI_API_KEY", "MEMINI_API_KEYS_FILE"},
	},
	{
		Key:   "namespace",
		Title: "Namespaces",
		Intro: "Which namespace a request reads and writes. See [namespace scoping](../scopes.md) for the " +
			"model.\n\n`MEMINI_NAMESPACE`, `MEMINI_DEFAULT_NAMESPACE` and `MEMINI_AGENT` are resolved outside the " +
			"configstruct (in `internal/config/namespace.go`), which is why they carry a source note rather than a " +
			"Go field name.",
		Vars: []string{"MEMINI_DEFAULT_NAMESPACE", "MEMINI_NAMESPACE", "MEMINI_AGENT", "MEMINI_HOME", "MEMINI_CLIENT_DEFAULTS"},
	},
}

// extraVar is a setting read with os.Getenv rather than through an env tag, so
// the struct walk cannot see it. These are real, user-facing knobs; leaving them
// out is how MEMINI_DEFAULT_NAMESPACE ended up documented only in the Helm
// chart. checkGetenvCoverage keeps this list honest.
type extraVar struct {
	Name    string
	Default string
	Source  string
	Doc     string
}

var extraVars = []extraVar{
	{
		Name:    "MEMINI_DEFAULT_NAMESPACE",
		Default: "auto",
		Source:  nsSource,
		Doc: "The namespace used when a request carries no `X-Memini-Namespace` header. " +
			"When unset the server resolves one at startup: this variable, then the git " +
			"repository name of its working directory, then the directory name, then the " +
			"literal `default`. The resolved value and its source are logged at boot.\n\n" +
			"In HTTP mode this fallback is usually not what you want, because the server " +
			"runs detached from the agent's working directory and will resolve its own " +
			"project rather than the caller's. Install the plugin, or send the header " +
			"explicitly. In stdio mode the server inherits the agent's working directory, " +
			"so the fallback is correct.",
	},
	{
		Name:    "MEMINI_NAMESPACE",
		Default: "",
		Source:  nsSource,
		Doc: "An alias for `MEMINI_DEFAULT_NAMESPACE`, checked second. It exists because " +
			"the agent-side plugin reads the same variable to decide which namespace to " +
			"send, so exporting it once configures both sides.\n\n" +
			"A value containing `/` is preserved as written (`acme/phoenix` stays " +
			"`acme/phoenix`). Only git- and directory-derived names are reduced to a " +
			"basename.",
	},
	{
		Name:    "MEMINI_AGENT",
		Default: "",
		Source:  nsSource,
		Doc: "Nests the resolved namespace under a per-agent segment, so several agents " +
			"sharing one project get their own partitions (`acme/phoenix` becomes " +
			"`acme/phoenix/reviewer`). Reads still see the parent through the ancestor " +
			"cascade, so shared project knowledge is not duplicated.\n\n" +
			"**Applied by the agent-side plugin, not by the server.** The server's own " +
			"header-less fallback ignores it, so under a bare `memini mcp` with no plugin " +
			"this has no effect and you want `MEMINI_NAMESPACE` set to the full path " +
			"instead. `memini doctor` resolves it both ways and flags the divergence. See " +
			"[multi-agent namespaces](../guides/multi-agent-namespaces.md).",
	},
}

// deprecated mirrors one entry of config.go's deprecatedVars table.
type deprecated struct {
	Name     string
	Guidance string
	Fatal    bool
}

// field is one env-tagged member of config.Config.
type field struct {
	Env     string
	Default string
	HasDflt bool
	Type    string
	GoName  string
	Doc     string
}

func main() {
	var src, out, mcpOut, specPath, restOut string
	flag.StringVar(&src, "src", "internal/config/config.go", "path to config.go")
	flag.StringVar(&out, "out", "docs/reference/configuration.md", "configuration reference output")
	flag.StringVar(&mcpOut, "mcp-out", "", "MCP tool reference output; empty skips it")
	flag.StringVar(&specPath, "spec", "api/openapi.yaml", "path to the OpenAPI spec")
	flag.StringVar(&restOut, "rest-out", "", "REST reference output; empty skips it")
	flag.Parse()

	if err := run(src, out); err != nil {
		fmt.Fprintln(os.Stderr, "gendocs:", err)
		os.Exit(1)
	}
	if mcpOut != "" {
		if err := genMCP(mcpOut); err != nil {
			fmt.Fprintln(os.Stderr, "gendocs:", err)
			os.Exit(1)
		}
	}
	if restOut != "" {
		if err := genREST(specPath, restOut); err != nil {
			fmt.Fprintln(os.Stderr, "gendocs:", err)
			os.Exit(1)
		}
	}
}

func run(src, out string) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, parser.ParseComments)
	if err != nil {
		return err
	}

	fields, err := parseFields(fset, f)
	if err != nil {
		return err
	}
	deps, err := parseDeprecated(f)
	if err != nil {
		return err
	}
	if err := checkGroups(fields); err != nil {
		return err
	}
	if err := checkGetenvCoverage(filepath.Dir(src), fields, deps); err != nil {
		return err
	}

	md := render(fields, deps)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(out, []byte(md), 0o644)
}

// parseFields walks the Config struct and collects every field carrying an env
// tag, together with its doc comment.
func parseFields(fset *token.FileSet, f *ast.File) ([]field, error) {
	var fields []field
	var missing []string

	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Config" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return false
		}
		for _, fl := range st.Fields.List {
			if fl.Tag == nil || len(fl.Names) == 0 {
				continue
			}
			raw, err := strconv.Unquote(fl.Tag.Value)
			if err != nil {
				continue
			}
			tag := reflect.StructTag(raw)
			env := tag.Get("env")
			if env == "" {
				continue
			}
			dflt, hasDflt := tag.Lookup("envDefault")

			doc := fieldDoc(fl, fl.Names[0].Name)
			if doc == "" {
				missing = append(missing, env)
			}

			fields = append(fields, field{
				Env:     env,
				Default: dflt,
				HasDflt: hasDflt,
				Type:    exprString(fset, fl.Type),
				GoName:  fl.Names[0].Name,
				Doc:     doc,
			})
		}
		return false
	})

	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("these settings have no doc comment, so they cannot be documented:\n  %s\n"+
			"Add a doc comment on the field in config.go. The comment is the documentation",
			strings.Join(missing, "\n  "))
	}
	return fields, nil
}

// fieldDoc extracts a field's own documentation, which is not simply its
// preceding comment. Go attaches a section header ("// Storage.") to whatever
// field happens to sit below it, so a naive read gives MEMINI_BACKEND the
// description "Storage." and MEMINI_LLM_API_KEY the description of the LLM
// block as a whole.
//
// The Go convention that a doc comment opens with the identifier it documents
// is what disambiguates the two. We take the comment from the first line that
// opens with the field name, and treat a field with no such line as
// undocumented, which is what forces a real description to be written.
func fieldDoc(fl *ast.Field, goName string) string {
	text := fl.Doc.Text()
	if text == "" {
		text = fl.Comment.Text()
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == goName || strings.HasPrefix(t, goName+" ") ||
			strings.HasPrefix(t, goName+",") || strings.HasPrefix(t, goName+"/") {
			return strings.TrimSpace(strings.Join(lines[i:], "\n"))
		}
	}
	return ""
}

// parseDeprecated reads the deprecatedVars composite literal. The two fatal
// entries build their guidance by concatenating string literals, so constant
// folding is required rather than reading a single BasicLit.
func parseDeprecated(f *ast.File) ([]deprecated, error) {
	var out []deprecated

	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) == 0 || vs.Names[0].Name != "deprecatedVars" {
			return true
		}
		if len(vs.Values) == 0 {
			return false
		}
		lit, ok := vs.Values[0].(*ast.CompositeLit)
		if !ok {
			return false
		}
		for _, el := range lit.Elts {
			e, ok := el.(*ast.CompositeLit)
			if !ok || len(e.Elts) != 3 {
				continue
			}
			name, err1 := foldString(e.Elts[0])
			guidance, err2 := foldString(e.Elts[1])
			if err1 != nil || err2 != nil {
				continue
			}
			fatal := false
			if id, ok := e.Elts[2].(*ast.Ident); ok && id.Name == "true" {
				fatal = true
			}
			out = append(out, deprecated{Name: name, Guidance: guidance, Fatal: fatal})
		}
		return false
	})

	if len(out) == 0 {
		return nil, fmt.Errorf("could not read deprecatedVars from config.go; has it been renamed?")
	}
	return out, nil
}

// foldString evaluates a string literal or any tree of `+` concatenations of
// string literals.
func foldString(e ast.Expr) (string, error) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", fmt.Errorf("not a string literal")
		}
		return strconv.Unquote(v.Value)
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", fmt.Errorf("unsupported operator %v", v.Op)
		}
		l, err := foldString(v.X)
		if err != nil {
			return "", err
		}
		r, err := foldString(v.Y)
		if err != nil {
			return "", err
		}
		return l + r, nil
	}
	return "", fmt.Errorf("unsupported expression")
}

// checkGroups is the gate. Every documented setting must belong to a section,
// and every section must name a setting that exists. Without this, a new field
// lands in the code and quietly never reaches the docs, which is precisely how
// MEMINI_STABILITY_K changed the default recall ranking undocumented.
func checkGroups(fields []field) error {
	inGroup := map[string]string{}
	for _, g := range groups {
		for _, v := range g.Vars {
			if prev, dup := inGroup[v]; dup {
				return fmt.Errorf("%s is listed in both the %q and %q groups", v, prev, g.Key)
			}
			inGroup[v] = g.Key
		}
	}

	known := map[string]bool{}
	for _, e := range extraVars {
		known[e.Name] = true
	}
	var ungrouped []string
	for _, f := range fields {
		known[f.Env] = true
		if _, ok := inGroup[f.Env]; !ok {
			ungrouped = append(ungrouped, f.Env)
		}
	}

	var unknown []string
	for v := range inGroup {
		if !known[v] {
			unknown = append(unknown, v)
		}
	}

	var problems []string
	if len(ungrouped) > 0 {
		sort.Strings(ungrouped)
		problems = append(problems, fmt.Sprintf(
			"these settings exist in config.go but belong to no section, so they would not appear in the docs:\n  %s\n"+
				"Add each to the groups table in cmd/gendocs/main.go",
			strings.Join(ungrouped, "\n  ")))
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		problems = append(problems, fmt.Sprintf(
			"the groups table names settings that no longer exist:\n  %s\n"+
				"Remove them from cmd/gendocs/main.go, or add them to deprecatedVars in config.go",
			strings.Join(unknown, "\n  ")))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "\n\n"))
	}
	return nil
}

var getenvRe = regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\("(MEMINI_[A-Z0-9_]+)"\)`)

// checkGetenvCoverage catches a setting read directly from the environment
// inside the config package without an env tag. Three already exist
// (MEMINI_DEFAULT_NAMESPACE, MEMINI_NAMESPACE, MEMINI_AGENT); a fourth added
// later must be declared in extraVars or this fails.
func checkGetenvCoverage(dir string, fields []field, deps []deprecated) error {
	known := map[string]bool{}
	for _, f := range fields {
		known[f.Env] = true
	}
	for _, e := range extraVars {
		known[e.Name] = true
	}
	for _, d := range deps {
		known[d.Name] = true
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var undeclared []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		for _, m := range getenvRe.FindAllStringSubmatch(string(b), -1) {
			if !known[m[1]] {
				undeclared = append(undeclared, m[1]+" (read in "+name+")")
				known[m[1]] = true
			}
		}
	}
	if len(undeclared) > 0 {
		sort.Strings(undeclared)
		return fmt.Errorf("these settings are read from the environment but declared nowhere the docs can see:\n  %s\n"+
			"Add each to extraVars in cmd/gendocs/main.go", strings.Join(undeclared, "\n  "))
	}
	return nil
}

// anchor is the slug GitHub and Forgejo both derive from a `### `+"`NAME`"
// heading: backticks stripped, lowercased, underscores kept.
func anchor(env string) string { return "#" + strings.ToLower(env) }

func defaultCell(f field) string {
	if !f.HasDflt || f.Default == "" {
		return noDefault
	}
	return "`" + f.Default + "`"
}

func render(fields []field, deps []deprecated) string {
	byEnv := map[string]field{}
	for _, f := range fields {
		byEnv[f.Env] = f
	}
	extraByEnv := map[string]extraVar{}
	for _, e := range extraVars {
		extraByEnv[e.Name] = e
	}
	// Field name to env var, for rewriting Go prose into docs prose.
	goToEnv := map[string]string{}
	for _, f := range fields {
		goToEnv[f.GoName] = f.Env
	}

	var b strings.Builder
	b.WriteString("<!-- Generated by cmd/gendocs from internal/config/config.go. DO NOT EDIT. -->\n")
	b.WriteString("<!-- Regenerate with: mise run docs -->\n\n")
	b.WriteString("# Configuration reference\n\n")
	b.WriteString("Every setting the memini **server** reads, generated from the code so it cannot go stale.\n\n")
	b.WriteString("> This page is server-side only. The agent-side plugin reads its own, separate set of\n")
	b.WriteString("> variables, and four names appear on both sides meaning different things. If you are not\n")
	b.WriteString("> sure which side you are configuring, start at [server vs client variables](env-vars.md).\n\n")
	b.WriteString("## Minimum to run\n\n")
	b.WriteString("memini starts with no configuration at all, on an embedded SQLite file. Vector search is\n")
	b.WriteString("the one thing it cannot invent, so point it at an embeddings endpoint:\n\n")
	b.WriteString("```sh\n" +
		"export MEMINI_EMBED_BASE_URL=http://localhost:8081/v1\n" +
		"export MEMINI_EMBED_MODEL=bge-m3\n" +
		"export MEMINI_EMBED_DIMS=1024   # must match the model\n" +
		"```\n\n")
	b.WriteString("Everything else has a working default. Add `MEMINI_LLM_BASE_URL` to turn on the\n")
	b.WriteString("consolidation pipeline, and `MEMINI_POSTGRES_DSN` with `MEMINI_BACKEND=postgres` for a\n")
	b.WriteString("server deployment. Treat the rest as tuning you reach for when you have a reason.\n\n")

	// Index.
	b.WriteString("## All settings\n\n")
	b.WriteString("| Setting | Default | Section |\n| --- | --- | --- |\n")
	for _, g := range groups {
		for _, v := range g.Vars {
			var dflt string
			if f, ok := byEnv[v]; ok {
				dflt = defaultCell(f)
			} else if e, ok := extraByEnv[v]; ok {
				dflt = "`" + e.Default + "`"
				if e.Default == "" {
					dflt = noDefault
				}
			}
			fmt.Fprintf(&b, "| [`%s`](%s) | %s | [%s](#%s) |\n", v, anchor(v), dflt, g.Title, slugTitle(g.Title))
		}
	}
	b.WriteString("\n")

	// Sections.
	for _, g := range groups {
		fmt.Fprintf(&b, "## %s\n\n", g.Title)
		if g.Intro != "" {
			b.WriteString(g.Intro + "\n\n")
		}
		for _, v := range g.Vars {
			fmt.Fprintf(&b, "### `%s`\n\n", v)
			if f, ok := byEnv[v]; ok {
				fmt.Fprintf(&b, "%s, default %s. Set by `Config.%s`.\n\n", f.Type, defaultCell(f), f.GoName)
				b.WriteString(renderDoc(f.Doc, f.GoName, v, goToEnv) + "\n\n")
				continue
			}
			e := extraByEnv[v]
			d := "`" + e.Default + "`"
			if e.Default == "" {
				d = noDefault
			}
			fmt.Fprintf(&b, "string, default %s. Resolved in `%s`.\n\n", d, e.Source)
			b.WriteString(escapeAngles(e.Doc) + "\n\n")
		}
	}

	// Deprecations.
	b.WriteString("## Removed settings\n\n")
	b.WriteString("These no longer exist. Two of them **refuse the boot**, because the change underneath\n")
	b.WriteString("them was not safe to ignore silently. The rest are ignored with a startup warning, so an\n")
	b.WriteString("old tuning value quietly stops applying. If you are upgrading, read\n")
	b.WriteString("[upgrading](../operations/upgrading.md).\n\n")

	b.WriteString("### The server will not start\n\n")
	b.WriteString("| Setting | What to do |\n| --- | --- |\n")
	for _, d := range deps {
		if d.Fatal {
			fmt.Fprintf(&b, "| `%s` | %s |\n", d.Name, tableCell(d.Guidance))
		}
	}
	b.WriteString("\n### Ignored, with a warning\n\n")
	b.WriteString("| Setting | What replaced it |\n| --- | --- |\n")
	for _, d := range deps {
		if !d.Fatal {
			fmt.Fprintf(&b, "| `%s` | %s |\n", d.Name, tableCell(d.Guidance))
		}
	}
	b.WriteString("\n")
	return b.String()
}

// slugTitle mirrors the forge heading slug for a plain title.
func slugTitle(s string) string {
	s = strings.ToLower(s)
	s = regexp.MustCompile(`[^a-z0-9 -]`).ReplaceAllString(s, "")
	return strings.ReplaceAll(s, " ", "-")
}

// tableCell flattens prose so it cannot break out of a markdown table. The
// pipe escape matters: the guidance strings mention hash|secret and similar.
func tableCell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = escapeAngles(s)
	s = strings.ReplaceAll(s, "|", `\|`)
	return docPathRe.ReplaceAllString(s, "[docs/$1.md$2](../$1.md$2)")
}

// escapeAngles protects the angle brackets that appear in placeholders such as
// <tenant>/_shared and `memini link add <ns> <old-global>`, which a markdown
// renderer would otherwise swallow as HTML tags. Anything already inside
// backticks is left alone.
func escapeAngles(s string) string {
	var b strings.Builder
	inCode := false
	for _, r := range s {
		switch {
		case r == '`':
			inCode = !inCode
			b.WriteRune(r)
		case r == '<' && !inCode:
			b.WriteString("&lt;")
		case r == '>' && !inCode:
			b.WriteString("&gt;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// multiHump reports whether an identifier is unambiguously a Go field name
// rather than an ordinary English word. Config has fields called Home, Cascade,
// Backend and Rerank, all of which appear as plain words in the prose, so
// rewriting every identifier would produce nonsense. Requiring two capitals and
// some length keeps WriteDedupAction and RecallSemanticReserve while leaving
// Home and Rerank alone.
func multiHump(s string) bool {
	if len(s) < 8 {
		return false
	}
	caps := 0
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			caps++
		}
	}
	return caps >= 2
}

// renderDoc turns a Go doc comment into docs prose. The comments are written
// for a Go reader ("WriteDedupScore is the fused similarity ... see
// WriteDedupAction"), so the leading field name and any unambiguous references
// to sibling fields are rewritten to the env var the reader actually sets.
func renderDoc(doc, goName, env string, goToEnv map[string]string) string {
	// "FieldName is the ..." reads wrong under a heading of MEMINI_FIELD_NAME.
	// The name is followed by punctuation often enough to matter ("UIAddr, when
	// set ...", "RerankModel / RerankAPIKey ...").
	for _, sep := range []string{" ", ",", "/"} {
		if strings.HasPrefix(doc, goName+sep) {
			doc = "`" + env + "`" + doc[len(goName):]
			break
		}
	}

	var p comment.Parser
	parsed := p.Parse(doc)

	var b strings.Builder
	for i, blk := range parsed.Content {
		if i > 0 {
			b.WriteString("\n\n")
		}
		switch v := blk.(type) {
		case *comment.Paragraph:
			b.WriteString(rewrite(plainText(v.Text), goToEnv))
		case *comment.Code:
			// The API keys YAML example lives in an indented block; fencing it
			// is the only thing that keeps it readable.
			b.WriteString("```yaml\n" + strings.TrimRight(v.Text, "\n") + "\n```")
		case *comment.List:
			for j, it := range v.Items {
				if j > 0 {
					b.WriteString("\n")
				}
				var parts []string
				for _, c := range it.Content {
					if para, ok := c.(*comment.Paragraph); ok {
						parts = append(parts, plainText(para.Text))
					}
				}
				b.WriteString("- " + rewrite(strings.Join(parts, " "), goToEnv))
			}
		case *comment.Heading:
			b.WriteString("**" + rewrite(plainText(v.Text), goToEnv) + "**")
		}
	}
	return b.String()
}

// plainText flattens a parsed comment's inline nodes back to source text. The
// stdlib markdown printer escapes backticks, which would wreck the inline code
// the comments already use, so we keep the text verbatim and escape only what
// markdown genuinely requires.
func plainText(ts []comment.Text) string {
	var b strings.Builder
	for _, t := range ts {
		switch v := t.(type) {
		case comment.Plain:
			b.WriteString(string(v))
		case comment.Italic:
			b.WriteString(string(v))
		case *comment.Link:
			b.WriteString(plainText(v.Text))
		case *comment.DocLink:
			b.WriteString(plainText(v.Text))
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

var docPathRe = regexp.MustCompile(`docs/([a-z-]+)\.md(#[a-z0-9-]+)?`)

// rewrite maps Go identifiers onto the env vars a reader sets, and turns
// repository-relative doc paths into links that resolve from docs/reference/.
func rewrite(s string, goToEnv map[string]string) string {
	s = escapeAngles(s)
	for goName, env := range goToEnv {
		if !multiHump(goName) {
			continue
		}
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(goName) + `\b`)
		s = re.ReplaceAllString(s, "`"+env+"`")
	}
	return docPathRe.ReplaceAllString(s, "[docs/$1.md$2](../$1.md$2)")
}

func exprString(fset *token.FileSet, e ast.Expr) string {
	var b bytes.Buffer
	if err := printer.Fprint(&b, fset, e); err != nil {
		return ""
	}
	switch s := b.String(); s {
	case "time.Duration":
		return "duration"
	case "Backend":
		return "string"
	default:
		return s
	}
}
