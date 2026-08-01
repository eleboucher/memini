package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"

	"github.com/eleboucher/memini/internal/memory"
	"github.com/eleboucher/memini/internal/store"
)

const memoryColumns = `id, namespace, tier, content, summary, metadata, tags, importance,
	created_at, updated_at, last_accessed_at, access_count, expires_at, superseded_by,
	valid_from, valid_to, confidence, level, linked_memory_ids`

// Store is a Postgres/VectorChord backed store.Store.
type Store struct {
	pool    *pgxpool.Pool
	dims    int
	metrics store.Metrics
}

var _ store.Store = (*Store)(nil)

// SetMetrics installs an observability sink. Passing nil disables metrics.
func (s *Store) SetMetrics(m store.Metrics) {
	if m == nil {
		s.metrics = store.NopMetrics()
		return
	}
	s.metrics = m
}

// Open connects to Postgres, ensures the schema exists for the given embedding
// dimensionality, and returns a ready Store.
func Open(ctx context.Context, dsn string, dims int) (*Store, error) {
	if dims <= 0 {
		return nil, fmt.Errorf("postgres: dims must be positive, got %d", dims)
	}

	// Migrate on a single connection first: CREATE EXTENSION must run before the
	// pgvector type can be registered on pooled connections.
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if err := migrate(ctx, conn, dims); err != nil {
		_ = conn.Close(ctx)
		return nil, err
	}
	_ = conn.Close(ctx)

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse dsn: %w", err)
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, c)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: pool: %w", err)
	}
	return &Store{pool: pool, dims: dims, metrics: store.NopMetrics()}, nil
}

func migrate(ctx context.Context, conn *pgx.Conn, dims int) error {
	stmts := []string{
		`CREATE EXTENSION IF NOT EXISTS vchord CASCADE`,
		`CREATE OR REPLACE FUNCTION memini_tags_to_text(text[]) RETURNS text
			LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
		AS $$ SELECT array_to_string($1, ' ') $$`,
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS memories (
			id               text PRIMARY KEY,
			namespace        text NOT NULL,
			tier             text NOT NULL,
			content          text NOT NULL,
			summary          text NOT NULL DEFAULT '',
			metadata         jsonb NOT NULL DEFAULT '{}',
			tags             text[] NOT NULL DEFAULT '{}',
			importance       double precision NOT NULL DEFAULT 0,
			created_at       timestamptz NOT NULL,
			updated_at       timestamptz NOT NULL,
			last_accessed_at timestamptz NOT NULL,
			access_count     integer NOT NULL DEFAULT 0,
			expires_at       timestamptz,
			superseded_by    text,
			valid_from       timestamptz,
			valid_to         timestamptz,
			confidence       double precision,
			fingerprint      text NOT NULL DEFAULT '',
			level            text NOT NULL DEFAULT '',
			linked_memory_ids text NOT NULL DEFAULT '[]',
			embedding        vector(%d),
			fts              tsvector GENERATED ALWAYS AS (
				to_tsvector('english',
					content || ' ' || summary || ' ' || memini_tags_to_text(tags))
			) STORED
		)`, dims),
		`CREATE INDEX IF NOT EXISTS idx_memories_namespace ON memories(namespace)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_expires ON memories(expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_fts ON memories USING gin(fts)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_vec ON memories USING vchordrq (embedding vector_l2_ops)`,
		// Chunked embedding (see chunks.go): additive and FK-cascaded, so a
		// store that never enables it just carries an empty table, and an older
		// binary against this schema ignores it entirely.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS memory_chunks (
			memory_id  text NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
			chunk_idx  integer NOT NULL,
			namespace  text NOT NULL,
			text       text NOT NULL DEFAULT '',
			embedding  vector(%d) NOT NULL,
			PRIMARY KEY (memory_id, chunk_idx)
		)`, dims),
		`CREATE INDEX IF NOT EXISTS idx_memory_chunks_ns ON memory_chunks(namespace)`,
		// Mirrors idx_memories_vec so the chunk KNN gets the same plan.
		`CREATE INDEX IF NOT EXISTS idx_memory_chunks_vec ON memory_chunks USING vchordrq (embedding vector_l2_ops)`,
		// ListUnchunked/CountUnchunked filter on char_length(content) every
		// backfill tick, forever; without this expression index each tick is a
		// full-table scan computing the length of every content value.
		`CREATE INDEX IF NOT EXISTS idx_memories_content_len ON memories ((char_length(content)))`,
		// Backfill temporal-validity, confidence, fingerprint, and level columns on
		// databases created before them.
		`ALTER TABLE memory_chunks ADD COLUMN IF NOT EXISTS text text NOT NULL DEFAULT ''`,
		`ALTER TABLE memories ADD COLUMN IF NOT EXISTS valid_from timestamptz`,
		`ALTER TABLE memories ADD COLUMN IF NOT EXISTS valid_to timestamptz`,
		`ALTER TABLE memories ADD COLUMN IF NOT EXISTS confidence double precision`,
		`ALTER TABLE memories ADD COLUMN IF NOT EXISTS fingerprint text NOT NULL DEFAULT ''`,
		`ALTER TABLE memories ADD COLUMN IF NOT EXISTS level text NOT NULL DEFAULT ''`,
		// Allow a NULL embedding for vectorless rows (the write path used when
		// embedding generation is unavailable — see Upsert). Idempotent: a no-op
		// on a column that is already nullable, so this is safe on every Open.
		`ALTER TABLE memories ALTER COLUMN embedding DROP NOT NULL`,
		`ALTER TABLE memories ADD COLUMN IF NOT EXISTS linked_memory_ids text NOT NULL DEFAULT '[]'`,
		`CREATE INDEX IF NOT EXISTS idx_memories_fingerprint ON memories(namespace, tier, fingerprint)`,
		// Deferred-repair state (see store.RepairStore). On the memory rather
		// than in a job table so a degraded write's "still owes a vector" state
		// commits in the same transaction as the write itself — there is no
		// enqueue to lose. '' is store.RepairNone, so existing rows migrate into
		// "healthy" for free and an older binary ignores the columns.
		`ALTER TABLE memories ADD COLUMN IF NOT EXISTS embed_state text NOT NULL DEFAULT ''`,
		`ALTER TABLE memories ADD COLUMN IF NOT EXISTS embed_attempts integer NOT NULL DEFAULT 0`,
		`ALTER TABLE memories ADD COLUMN IF NOT EXISTS embed_next_run_at timestamptz`,
		`ALTER TABLE memories ADD COLUMN IF NOT EXISTS embed_last_error text NOT NULL DEFAULT ''`,
		// The repair claim runs on every poll tick forever, so it must be an
		// index range scan over just the outstanding repairs. Partial, so
		// repaired rows leave the index and it stays small permanently.
		`CREATE INDEX IF NOT EXISTS idx_memories_repair
			ON memories(embed_next_run_at, id) WHERE embed_state <> ''`,
		// Key/value store for store-level metadata (e.g. the embedding model the
		// vectors were produced with — see EmbedModel/SetEmbedModel).
		`CREATE TABLE IF NOT EXISTS meta (key text PRIMARY KEY, value text NOT NULL)`,
		// namespace_links records cross-namespace read links (namespace-cascade
		// design, see store.LinkStore); tiers is a JSON array of memory.Tier
		// strings.
		`CREATE TABLE IF NOT EXISTS namespace_links (
			src_ns     text NOT NULL,
			dst_ns     text NOT NULL,
			tiers      jsonb NOT NULL DEFAULT '[]',
			note       text NOT NULL DEFAULT '',
			created_at timestamptz NOT NULL,
			PRIMARY KEY (src_ns, dst_ns)
		)`,
		// api_keys records API credentials (see store.APIKeyStore): key_hash is
		// the hex SHA-256 of the secret (the secret itself is never stored) and
		// is UNIQUE so GetAPIKeyByHash's auth-path lookup can rely on an index.
		`CREATE TABLE IF NOT EXISTS api_keys (
			name       text PRIMARY KEY,
			key_hash   text NOT NULL UNIQUE,
			home_ns    text NOT NULL DEFAULT '',
			default_ns text NOT NULL DEFAULT '',
			created_at timestamptz NOT NULL,
			disabled   boolean NOT NULL DEFAULT false,
			admin      boolean NOT NULL DEFAULT false,
			read_only  boolean NOT NULL DEFAULT false
		)`,
		// Backfill default_ns on databases whose api_keys table predates it.
		`ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS default_ns text NOT NULL DEFAULT ''`,
		// api_keys.settings holds the per-key store.ClientSettings override
		// (config-handshake redesign) as jsonb; '{}' means no override.
		`ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS settings jsonb NOT NULL DEFAULT '{}'`,
		// api_keys.admin holds the per-key admin capability (admin-keys
		// redesign); see store.APIKey.Admin's doc.
		`ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS admin boolean NOT NULL DEFAULT false`,
		// api_keys.read_only holds the per-key read-only capability (see
		// store.APIKey.ReadOnly). Defaulting to false is what keeps every
		// pre-existing key read-write across the upgrade.
		`ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS read_only boolean NOT NULL DEFAULT false`,
		// memory_events is the activity log (see store.EventLogStore): one row
		// per (operation, memory), rows of one operation sharing op_id. The
		// memory_* columns are a snapshot, not a join — they keep the feed a
		// single query and keep a forget event readable after its memory is gone.
		`CREATE TABLE IF NOT EXISTS memory_events (
			id             bigserial PRIMARY KEY,
			op_id          text NOT NULL,
			kind           text NOT NULL,
			namespace      text NOT NULL,
			query          text NOT NULL DEFAULT '',
			memory_id      text NOT NULL DEFAULT '',
			memory_ns      text NOT NULL DEFAULT '',
			memory_tier    text NOT NULL DEFAULT '',
			memory_summary text NOT NULL DEFAULT '',
			rank           integer NOT NULL DEFAULT 0,
			score          double precision,
			detail         jsonb NOT NULL DEFAULT '{}',
			actor          text NOT NULL DEFAULT '',
			actor_kind     text NOT NULL DEFAULT '',
			created_at     timestamptz NOT NULL
		)`,
		// memory_events.actor/actor_kind carry activity attribution (admin-keys
		// T5): who performed each operation. Added after the table first
		// shipped, so ADD COLUMN IF NOT EXISTS for databases created before
		// attribution existed; a legacy row keeps the '' default, which renders
		// as "unknown" (see store.Event).
		`ALTER TABLE memory_events ADD COLUMN IF NOT EXISTS actor text NOT NULL DEFAULT ''`,
		`ALTER TABLE memory_events ADD COLUMN IF NOT EXISTS actor_kind text NOT NULL DEFAULT ''`,
		// The read path is always newest-first, optionally narrowed by namespace;
		// the (created_at DESC, id DESC) tail matches ListEvents' ordering so the
		// keyset cursor walks the index.
		`CREATE INDEX IF NOT EXISTS idx_memory_events_ns_time ON memory_events(namespace, created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_events_time ON memory_events(created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_events_memory ON memory_events(memory_id)`,
		// pins was named project_map until the terminology cleanup; rename an
		// existing table and its index in place, before the CREATE below can
		// plant a fresh empty pins next to it, so old installs keep their
		// rows. The exception guard tolerates a concurrent migrate (another
		// replica or the CLI) winning the rename between the check and the
		// ALTER. If both tables exist — an old binary re-created project_map
		// after the rename (a rollback window) and may have written pins into
		// it — the stray's rows are folded into pins and the stray dropped,
		// so no pin silently vanishes; on a key conflict the stray row wins
		// (the later write, the same last-write-wins rule as PutPins) while
		// created_at/created_by keep the pins row's values, as PutPins does.
		`DO $$
		BEGIN
			IF to_regclass('project_map') IS NOT NULL AND to_regclass('pins') IS NULL THEN
				BEGIN
					ALTER TABLE project_map RENAME TO pins;
				EXCEPTION WHEN undefined_table OR duplicate_table THEN
					NULL;
				END;
				IF to_regclass('idx_project_map_ns') IS NOT NULL AND to_regclass('idx_pins_ns') IS NULL THEN
					ALTER INDEX idx_project_map_ns RENAME TO idx_pins_ns;
				END IF;
			END IF;
			IF to_regclass('project_map') IS NOT NULL AND to_regclass('pins') IS NOT NULL THEN
				INSERT INTO pins (key, namespace, note, created_by, created_at, updated_at)
					SELECT key, namespace, note, created_by, created_at, updated_at FROM project_map
					ON CONFLICT (key) DO UPDATE SET
						namespace=EXCLUDED.namespace, note=EXCLUDED.note, updated_at=EXCLUDED.updated_at;
				DROP TABLE project_map;
			END IF;
		END
		$$`,
		// pins records project→namespace pins (see store.PinStore,
		// the config-handshake redesign): key is "remote:<canonical-remote>" or
		// "path:<absolute-toplevel>".
		`CREATE TABLE IF NOT EXISTS pins (
			key        text PRIMARY KEY,
			namespace  text NOT NULL,
			note       text NOT NULL DEFAULT '',
			created_by text NOT NULL DEFAULT '',
			created_at timestamptz NOT NULL,
			updated_at timestamptz NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pins_ns ON pins(namespace)`,
	}
	for _, q := range stmts {
		if _, err := conn.Exec(ctx, q); err != nil {
			return fmt.Errorf("postgres: migrate: %w\nstatement: %s", err, q)
		}
	}
	return nil
}

// Upsert inserts or replaces a memory. When m.Embedding is empty the row is
// stored with a NULL embedding (keyword index still written) — the write path
// used when embedding generation is unavailable; any other length must equal
// the store's dims. Returns ErrConflict when the ID already exists under a
// different namespace.
func (s *Store) Upsert(ctx context.Context, m *memory.Memory) error {
	if len(m.Embedding) != 0 && len(m.Embedding) != s.dims {
		return fmt.Errorf("postgres: embedding has %d dims, store expects %d", len(m.Embedding), s.dims)
	}
	metaJSON, err := json.Marshal(store.OrEmptyMap(m.Metadata))
	if err != nil {
		return fmt.Errorf("postgres: marshal metadata: %w", err)
	}
	// A literal Go nil (not a typed nil *pgvector.Vector) so pgx's nil-value
	// fast path encodes SQL NULL directly; pgvector.Vector.Value() has a value
	// receiver, so calling it through a nil *Vector would panic instead.
	var embArg any
	if len(m.Embedding) != 0 {
		embArg = pgvector.NewVector(m.Embedding)
	}
	linkedIDs := m.LinkedMemoryIDs
	if linkedIDs == nil {
		linkedIDs = []string{}
	}
	linkedJSON, err := json.Marshal(linkedIDs)
	if err != nil {
		return fmt.Errorf("postgres: marshal linked memory ids: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the existing row (if any) and verify namespace ownership.
	fp := memory.Fingerprint(m.Content)
	var existingNS, existingFP string
	var op string
	err = tx.QueryRow(ctx, `SELECT namespace, fingerprint FROM memories WHERE id=$1 FOR UPDATE`, m.ID).
		Scan(&existingNS, &existingFP)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		op = "insert"
	case err != nil:
		return fmt.Errorf("postgres: check existing namespace: %w", err)
	default:
		op = "update"
		if existingNS != m.Namespace {
			return fmt.Errorf("postgres: id %q exists in namespace %q: %w", m.ID, existingNS, store.ErrConflict)
		}
	}

	// The WHERE on the conflict update makes the ownership guard atomic with the
	// write: SELECT ... FOR UPDATE locks nothing when the row is absent, so two
	// concurrent inserts of the same id from different namespaces both reach
	// here. A foreign-namespace conflict then updates 0 rows (reported below as
	// ErrConflict) instead of overwriting the winner.
	tag, err := tx.Exec(ctx, `
		INSERT INTO memories
			(id, namespace, tier, content, summary, metadata, tags, importance,
			 created_at, updated_at, last_accessed_at, access_count, expires_at, superseded_by,
			 valid_from, valid_to, confidence, fingerprint, level, linked_memory_ids, embedding)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		ON CONFLICT (id) DO UPDATE SET
			tier=EXCLUDED.tier, content=EXCLUDED.content,
			summary=EXCLUDED.summary, metadata=EXCLUDED.metadata, tags=EXCLUDED.tags,
			importance=EXCLUDED.importance, updated_at=EXCLUDED.updated_at,
			last_accessed_at=EXCLUDED.last_accessed_at, access_count=EXCLUDED.access_count,
			expires_at=EXCLUDED.expires_at, superseded_by=EXCLUDED.superseded_by,
			valid_from=EXCLUDED.valid_from, valid_to=EXCLUDED.valid_to,
			confidence=EXCLUDED.confidence, fingerprint=EXCLUDED.fingerprint,
			level=EXCLUDED.level, linked_memory_ids=EXCLUDED.linked_memory_ids,
			embedding=EXCLUDED.embedding
		WHERE memories.namespace = EXCLUDED.namespace`,
		m.ID, m.Namespace, string(m.Tier), m.Content, m.Summary, metaJSON, store.OrEmptySlice(m.Tags),
		m.Importance, m.CreatedAt, m.UpdatedAt, m.LastAccessedAt, m.AccessCount,
		m.ExpiresAt, m.SupersededBy, m.ValidFrom, m.ValidTo, m.Confidence, fp,
		string(m.Level), linkedJSON,
		embArg)
	if err != nil {
		return fmt.Errorf("postgres: upsert: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Conflict on id but the existing row belongs to another namespace, so
		// the guarded update matched nothing.
		return fmt.Errorf("postgres: id %q exists in another namespace: %w", m.ID, store.ErrConflict)
	}
	// Same transaction as the row, so content and its chunk vectors are never
	// observed out of step. Memory.Chunks' contract: non-nil replaces, nil
	// preserves while the fingerprint is unchanged and clears when it is not.
	// Fresh rows have nothing to preserve or clear (the FK rules out orphans),
	// so an insert with nil chunks skips the write entirely.
	if m.Chunks != nil || (op == "update" && existingFP != fp) {
		if err := s.writeChunks(ctx, tx, m); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.metrics.Upsert(op, string(m.Tier), store.MemoryTypeLabel(m))
	return nil
}

// Get returns a memory by ID.
func (s *Store) Get(ctx context.Context, namespace, id string) (*memory.Memory, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+memoryColumns+` FROM memories WHERE id=$1 AND namespace=$2`, id, namespace)
	m, err := scanMemory(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return m, err
}

// IDsByPrefix returns the IDs in the namespace beginning with prefix,
// ascending, bounded at limit rows — an indexed prefix scan (LIKE has
// byte-wise prefix semantics on Postgres regardless of the database
// collation, and is case-sensitive). Metacharacters are escaped so prefix
// always matches literally.
func (s *Store) IDsByPrefix(ctx context.Context, namespace, prefix string, limit int) ([]string, error) {
	if prefix == "" || limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM memories WHERE namespace = $1 AND id LIKE $2 ESCAPE '\'
		 ORDER BY id LIMIT $3`,
		namespace, escapeLike(prefix)+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: ids by prefix %q: %w", prefix, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetEmbedding returns the stored vector for a memory, or nil when the row is
// vectorless (embedding IS NULL — a degraded write awaiting backfill). Scanning
// into a *pgvector.Vector rather than a value is what keeps those two cases
// apart: pgx leaves the pointer nil for SQL NULL, where a value scan would
// error.
func (s *Store) GetEmbedding(ctx context.Context, namespace, id string) ([]float32, error) {
	var emb *pgvector.Vector
	err := s.pool.QueryRow(ctx,
		`SELECT embedding FROM memories WHERE id=$1 AND namespace=$2`, id, namespace).Scan(&emb)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get embedding %q: %w", id, err)
	}
	if emb == nil {
		return nil, nil
	}
	return emb.Slice(), nil
}

// PredecessorIDs returns the IDs of memories in the namespace superseded by id.
func (s *Store) PredecessorIDs(ctx context.Context, namespace, id string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM memories WHERE namespace=$1 AND superseded_by=$2`, namespace, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var x string
		if err := rows.Scan(&x); err != nil {
			return nil, err
		}
		ids = append(ids, x)
	}
	return ids, rows.Err()
}

// GetByFingerprint returns the most recent live memory in namespace+tier whose
// content fingerprint matches. Superseded, expired, and validity-closed
// (contradicted) rows are excluded so a dead duplicate never absorbs a fresh
// write — re-asserting a contradicted fact must store a live row, not
// corroborate the invalidated one.
func (s *Store) GetByFingerprint(
	ctx context.Context, namespace string, tier memory.Tier, fingerprint string, now time.Time,
) (*memory.Memory, error) {
	if fingerprint == "" {
		return nil, store.ErrNotFound
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	row := s.pool.QueryRow(ctx,
		`SELECT `+memoryColumns+` FROM memories
		 WHERE namespace=$1 AND tier=$2 AND fingerprint=$3 AND superseded_by IS NULL
		   AND (expires_at IS NULL OR expires_at > $4)
		   AND (valid_to IS NULL OR valid_to > $4)
		 ORDER BY created_at DESC LIMIT 1`,
		namespace, string(tier), fingerprint, now)
	m, err := scanMemory(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return m, err
}

// DeleteIfExpiredBefore removes a memory only if its expiry is still at or
// before cutoff. Returns ErrNotFound when the memory is absent or its TTL was
// slid past cutoff by Reinforce since the last ListExpired call.
func (s *Store) DeleteIfExpiredBefore(ctx context.Context, namespace, id string, cutoff time.Time) error {
	var tier string
	err := s.pool.QueryRow(ctx,
		`DELETE FROM memories WHERE id=$1 AND namespace=$2 AND expires_at IS NOT NULL AND expires_at <= $3
		 RETURNING tier`,
		id, namespace, cutoff).Scan(&tier)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		return err
	}
	s.metrics.SweepExpired(tier)
	return nil
}

// Delete removes a memory by ID.
func (s *Store) Delete(ctx context.Context, namespace, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM memories WHERE id=$1 AND namespace=$2`, id, namespace)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	s.metrics.Delete()
	return nil
}

// SetSuperseded records that a memory was replaced by supersededBy, stamping
// valid_to at the moment of supersession (unless already set) so a time-filtered
// recall can still surface the fact for the window it held.
func (s *Store) SetSuperseded(ctx context.Context, namespace, id, supersededBy string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE memories SET superseded_by=$1, valid_to=COALESCE(valid_to, now()) WHERE id=$2 AND namespace=$3`,
		supersededBy, id, namespace)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	s.metrics.SoftDelete()
	return nil
}

// Restore clears superseded_by/valid_to so a tombstoned memory is live again.
func (s *Store) Restore(ctx context.Context, namespace, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE memories SET superseded_by=NULL, valid_to=NULL WHERE id=$1 AND namespace=$2`,
		id, namespace)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// Reinforce bumps access_count/last_accessed_at and optionally slides the TTL.
func (s *Store) Reinforce(ctx context.Context, namespace string, ids []string, accessedAt time.Time, newExpiry *time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	b := &args{}
	set := "access_count = access_count + 1, last_accessed_at = " + b.add(accessedAt)
	if newExpiry != nil {
		// Only slide rows that already expire (short-term); never add a TTL to durable rows.
		set += ", expires_at = CASE WHEN expires_at IS NOT NULL THEN " + b.add(*newExpiry) + " ELSE expires_at END"
	}
	ns := b.add(namespace)
	idsParam := b.add(ids)
	q := fmt.Sprintf("UPDATE memories SET %s WHERE namespace = %s AND id = ANY(%s)", set, ns, idsParam)
	_, err := s.pool.Exec(ctx, q, b.vals...)
	return err
}

// VectorSearch returns the k nearest live memories to vec in the namespace.
func (s *Store) VectorSearch(ctx context.Context, namespace string, vec []float32, f store.Filter, k int) ([]store.Scored, error) {
	if len(vec) != s.dims {
		return nil, fmt.Errorf("postgres: query vector has %d dims, store expects %d", len(vec), s.dims)
	}
	b := &args{}
	qv := b.add(pgvector.NewVector(vec))
	ns := b.add(namespace)
	where := filterClause(b, f)
	q := fmt.Sprintf(`
		SELECT %s, embedding <-> %s AS distance
		FROM memories
		WHERE namespace = %s AND embedding IS NOT NULL%s
		ORDER BY embedding <-> %s
		LIMIT %s`,
		memoryColumns, qv, ns, where, qv, b.add(k))
	return s.queryScored(ctx, q, b.vals, func(d float64) float64 { return 1 / (1 + d) })
}

// KeywordSearch returns the k best full-text matches in the namespace.
func (s *Store) KeywordSearch(ctx context.Context, namespace, query string, f store.Filter, k int) ([]store.Scored, error) {
	// OR the terms (term1 | term2 | ...) so recall matches rows containing ANY
	// term, rather than plainto_tsquery's all-terms AND.
	tsq := tsQuery(query)
	if tsq == "" {
		return nil, nil
	}
	b := &args{}
	qq := b.add(tsq)
	ns := b.add(namespace)
	where := filterClause(b, f)
	q := fmt.Sprintf(`
		SELECT %s, ts_rank(fts, to_tsquery('english', %s)) AS rank
		FROM memories
		WHERE fts @@ to_tsquery('english', %s) AND namespace = %s%s
		ORDER BY rank DESC
		LIMIT %s`,
		memoryColumns, qq, qq, ns, where, b.add(k))
	return s.queryScored(ctx, q, b.vals, func(r float64) float64 { return r })
}

// ListExpired returns up to limit memories whose TTL has passed.
func (s *Store) ListExpired(ctx context.Context, now time.Time, limit int) ([]*memory.Memory, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+memoryColumns+` FROM memories WHERE expires_at IS NOT NULL AND expires_at <= $1 LIMIT $2`,
		now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// List returns memories in a namespace matching f (without embeddings),
// ordered by f.Sort (newest-created first by default).
func (s *Store) List(ctx context.Context, namespace string, f store.Filter, limit int) ([]*memory.Memory, error) {
	b := &args{}
	ns := b.add(namespace)
	where := filterClause(b, f)
	q := fmt.Sprintf(`SELECT %s FROM memories WHERE namespace = %s%s%s`,
		memoryColumns, ns, where, orderClause(f.Sort))
	if limit > 0 {
		q += " LIMIT " + b.add(limit)
	}
	rows, err := s.pool.Query(ctx, q, b.vals...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListNamespaces returns the distinct namespaces holding memories.
func (s *Store) ListNamespaces(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT namespace FROM memories ORDER BY namespace`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return nil, err
		}
		out = append(out, ns)
	}
	return out, rows.Err()
}

// NamespaceActivity implements store.ActivityStore: one aggregate query for
// per-namespace live count and most recent created_at. Liveness reuses
// filterClause with an empty Filter so it stays byte-identical to what a
// default List applies (not expired at now, not superseded, validity window
// not closed).
func (s *Store) NamespaceActivity(ctx context.Context, now time.Time) ([]store.NamespaceActivity, error) {
	b := &args{}
	where := filterClause(b, store.Filter{Now: now})
	q := `SELECT namespace, COUNT(*), MAX(created_at) FROM memories WHERE true` +
		where + ` GROUP BY namespace ORDER BY namespace`
	rows, err := s.pool.Query(ctx, q, b.vals...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.NamespaceActivity
	for rows.Next() {
		var a store.NamespaceActivity
		var total int64
		if err := rows.Scan(&a.NS, &total, &a.LastWrite); err != nil {
			return nil, err
		}
		a.Total = int(total)
		a.LastWrite = a.LastWrite.UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteNamespace removes every memory in a namespace, plus any
// namespace_links row that references the namespace on either side (gap G5:
// a deleted namespace must not leave a dangling link). Returns the number of
// memories deleted.
func (s *Store) DeleteNamespace(ctx context.Context, namespace string) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Cascade the link table first: this must happen even when the namespace
	// holds no memories (a namespace can exist purely as a link endpoint).
	if _, err := tx.Exec(ctx,
		`DELETE FROM namespace_links WHERE src_ns=$1 OR dst_ns=$1`, namespace); err != nil {
		return 0, fmt.Errorf("postgres: delete namespace: cascade links: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM memories WHERE namespace=$1`, namespace)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	n := tag.RowsAffected()
	for range n {
		s.metrics.Delete()
	}
	return n, nil
}

// Reassign moves memories from fromNS to toNS. The fts column is generated and
// the vector index lives on the same row, so a single namespace UPDATE suffices.
// IDs absent from fromNS are not matched; IDs are globally unique so a move
// never collides in toNS.
func (s *Store) Reassign(ctx context.Context, fromNS string, ids []string, toNS string) (int64, error) {
	if len(ids) == 0 || fromNS == toNS {
		return 0, nil
	}
	// One transaction for both tables, exactly as DeleteNamespace does. Two
	// autocommit statements would let a failure between them strand the chunk
	// rows under the old namespace — invisible to the chunk KNN in either
	// namespace, and never repaired, because a memory that has chunk rows is
	// not re-listed by the backfill.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE memories SET namespace=$1 WHERE namespace=$2 AND id = ANY($3)`, toNS, fromNS, ids)
	if err != nil {
		return 0, err
	}
	// memory_chunks carries its own namespace (the chunk KNN filters on it
	// before the join). The FK cascades deletes, not a namespace change, so the
	// chunks must be moved explicitly.
	if _, err := tx.Exec(ctx,
		`UPDATE memory_chunks SET namespace=$1 WHERE namespace=$2 AND memory_id = ANY($3)`,
		toNS, fromNS, ids); err != nil {
		return 0, fmt.Errorf("postgres: reassign chunks: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Retier changes a memory's tier and expiry in place. Tier and expiry live only
// in the memories row (fts is generated from content, the vector index is on the
// same row), so no reindex is required.
func (s *Store) Retier(ctx context.Context, namespace, id string, tier memory.Tier, expiresAt *time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE memories SET tier=$1, expires_at=$2 WHERE id=$3 AND namespace=$4`,
		string(tier), expiresAt, id, namespace)
	if err != nil {
		return fmt.Errorf("postgres: retier: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// SetConfidence updates a memory's confidence and bumps updated_at to now.
// Confidence lives only in the memories row, so no reindex is needed.
// Validity-closed rows are skipped (ErrNotFound): corroboration must never
// regrow an invalidated fact, even when MarkContradicted lands between the
// caller's read and this write.
func (s *Store) SetConfidence(ctx context.Context, namespace, id string, confidence float64, now time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE memories SET confidence=$1, updated_at=$2
		 WHERE id=$3 AND namespace=$4 AND (valid_to IS NULL OR valid_to > $5)`,
		confidence, now, id, namespace, now)
	if err != nil {
		return fmt.Errorf("postgres: set confidence: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// MarkContradicted invalidates a durable fact a newer write contradicts. The
// SET expressions read the pre-update confidence column (Postgres evaluates the
// right-hand side against the old row), snapshotting it into metadata for audit
// and reversal before overwriting it.
func (s *Store) MarkContradicted(ctx context.Context, namespace, id, contradictedBy string, confidence float64, now time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE memories SET
			metadata=jsonb_set(
				jsonb_set(metadata, '{contradicted_by}', to_jsonb($1::text)),
				'{contradicted_prev_confidence}', coalesce(to_jsonb(confidence), 'null'::jsonb)),
			confidence=$2,
			valid_to=COALESCE(valid_to, $3),
			updated_at=$4
		 WHERE id=$5 AND namespace=$6`,
		contradictedBy, confidence, now, now, id, namespace)
	if err != nil {
		return fmt.Errorf("postgres: mark contradicted: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

const metaEmbedModel = "embed_model"

// EmbedModel returns the recorded embedding model name, or "" if none was set.
func (s *Store) EmbedModel(ctx context.Context) (string, error) {
	var v string
	err := s.pool.QueryRow(ctx, `SELECT value FROM meta WHERE key=$1`, metaEmbedModel).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("postgres: read embed model: %w", err)
	}
	return v, nil
}

// SetEmbedModel records the embedding model the stored vectors were produced with.
func (s *Store) SetEmbedModel(ctx context.Context, model string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO meta(key, value) VALUES($1, $2)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, metaEmbedModel, model)
	if err != nil {
		return fmt.Errorf("postgres: set embed model: %w", err)
	}
	return nil
}

var _ store.ClientSettingsStore = (*Store)(nil)

const metaClientSettingsDefaults = "client_settings_defaults"

// GlobalClientSettings returns the stored global default ClientSettings, or
// the zero value (every field nil) if none has been set yet.
func (s *Store) GlobalClientSettings(ctx context.Context) (store.ClientSettings, error) {
	var v string
	err := s.pool.QueryRow(ctx, `SELECT value FROM meta WHERE key=$1`, metaClientSettingsDefaults).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ClientSettings{}, nil
	}
	if err != nil {
		return store.ClientSettings{}, fmt.Errorf("postgres: read global client settings: %w", err)
	}
	var cs store.ClientSettings
	// Tolerant decode: an unknown field from a newer writer is ignored
	// (json.Unmarshal's default behavior) — strict validation is the REST
	// boundary's job, not the store's.
	if err := json.Unmarshal([]byte(v), &cs); err != nil {
		return store.ClientSettings{}, fmt.Errorf("postgres: unmarshal global client settings: %w", err)
	}
	return cs, nil
}

// SetGlobalClientSettings replaces the stored global default ClientSettings
// wholesale (not a merge): only fields set on s are persisted, since nil
// pointer fields with `omitempty` marshal to nothing.
func (s *Store) SetGlobalClientSettings(ctx context.Context, cs store.ClientSettings) error {
	b, err := json.Marshal(cs)
	if err != nil {
		return fmt.Errorf("postgres: marshal global client settings: %w", err)
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO meta(key, value) VALUES($1, $2)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, metaClientSettingsDefaults, string(b))
	if err != nil {
		return fmt.Errorf("postgres: set global client settings: %w", err)
	}
	return nil
}

var _ store.LinkStore = (*Store)(nil)

// PutLink inserts or replaces the link keyed by (l.Src, l.Dst).
//
// Unlike memory Put (created_at is immutable after insert), an upsert here
// overwrites created_at. This is intentional, not an oversight: links carry
// no recency semantics that a stable created_at would protect, and import
// restore relies on the overwrite being conditional on l.CreatedAt being
// non-zero (below) so it can replay a link's original creation time instead
// of stamping "now".
func (s *Store) PutLink(ctx context.Context, l store.NamespaceLink) error {
	tiersJSON, err := json.Marshal(tiersOrEmpty(l.Tiers))
	if err != nil {
		return fmt.Errorf("postgres: marshal link tiers: %w", err)
	}
	created := l.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO namespace_links (src_ns, dst_ns, tiers, note, created_at)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (src_ns, dst_ns) DO UPDATE SET
			tiers=EXCLUDED.tiers, note=EXCLUDED.note, created_at=EXCLUDED.created_at`,
		l.Src, l.Dst, tiersJSON, l.Note, created)
	if err != nil {
		return fmt.Errorf("postgres: put link: %w", err)
	}
	return nil
}

// DeleteLink removes the link from src to dst. The bool reports whether a
// link existed to delete.
func (s *Store) DeleteLink(ctx context.Context, src, dst string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM namespace_links WHERE src_ns=$1 AND dst_ns=$2`, src, dst)
	if err != nil {
		return false, fmt.Errorf("postgres: delete link: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListLinks returns the links whose Src is src, ordered by Dst.
func (s *Store) ListLinks(ctx context.Context, src string) ([]store.NamespaceLink, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT src_ns, dst_ns, tiers, note, created_at FROM namespace_links WHERE src_ns=$1 ORDER BY dst_ns`, src)
	if err != nil {
		return nil, fmt.Errorf("postgres: list links: %w", err)
	}
	return scanLinks(rows)
}

// ListAllLinks returns every link in the store, ordered by Src then Dst.
func (s *Store) ListAllLinks(ctx context.Context) ([]store.NamespaceLink, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT src_ns, dst_ns, tiers, note, created_at FROM namespace_links ORDER BY src_ns, dst_ns`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list all links: %w", err)
	}
	return scanLinks(rows)
}

// RenameLinkEndpoints rewrites every link whose src_ns or dst_ns equals from
// to to instead. When a rewritten link collides with a pre-existing row at
// its new key, the pre-existing row is kept and the renamed link dropped
// (ON CONFLICT DO NOTHING): the target namespace's own explicit grant wins
// over an inherited one, so a rename can never silently widen or narrow tier
// access the target had already configured. The SELECT is ordered by
// (src_ns, dst_ns) so which renamed link survives a multi-way collision
// (e.g. the reciprocal pair link(from,to)+link(to,from) collapsing onto
// (to,to)) is deterministic: the first row in key order wins. A no-op when
// from == to.
func (s *Store) RenameLinkEndpoints(ctx context.Context, from, to string) error {
	if from == to {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT src_ns, dst_ns, tiers, note, created_at FROM namespace_links
		 WHERE src_ns=$1 OR dst_ns=$1 ORDER BY src_ns, dst_ns`, from)
	if err != nil {
		return fmt.Errorf("postgres: rename link endpoints: select: %w", err)
	}
	type linkRow struct {
		src, dst, note string
		tiers          []byte
		created        time.Time
	}
	var toRename []linkRow
	for rows.Next() {
		var r linkRow
		if err := rows.Scan(&r.src, &r.dst, &r.tiers, &r.note, &r.created); err != nil {
			rows.Close()
			return err
		}
		toRename = append(toRename, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, r := range toRename {
		newSrc, newDst := r.src, r.dst
		if newSrc == from {
			newSrc = to
		}
		if newDst == from {
			newDst = to
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM namespace_links WHERE src_ns=$1 AND dst_ns=$2`, r.src, r.dst); err != nil {
			return fmt.Errorf("postgres: rename link endpoints: delete: %w", err)
		}
		// DO NOTHING, not DO UPDATE: a pre-existing link at the new key is the
		// target namespace's own configuration and must survive untouched.
		if _, err := tx.Exec(ctx, `INSERT INTO namespace_links (src_ns, dst_ns, tiers, note, created_at)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (src_ns, dst_ns) DO NOTHING`,
			newSrc, newDst, r.tiers, r.note, r.created); err != nil {
			return fmt.Errorf("postgres: rename link endpoints: insert: %w", err)
		}
	}
	return tx.Commit(ctx)
}

var _ store.APIKeyStore = (*Store)(nil)

// PutAPIKey inserts or replaces the key keyed by k.Name.
//
// Unlike PutLink (which deliberately overwrites created_at on every upsert,
// since links carry no recency semantics — see its doc above), this upsert
// preserves the existing row's created_at when k.CreatedAt is the zero
// value: API keys are long-lived identity, and rotating a key's hash or
// home namespace must not reset "when was this key first created". A
// non-zero k.CreatedAt (e.g. import restore replaying an original
// timestamp) still overwrites it. The lookup-then-upsert runs in a
// transaction so a concurrent PutAPIKey for the same name cannot race
// between the read of the existing created_at and the write.
func (s *Store) PutAPIKey(ctx context.Context, k store.APIKey) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	created := k.CreatedAt
	if created.IsZero() {
		err := tx.QueryRow(ctx, `SELECT created_at FROM api_keys WHERE name=$1`, k.Name).Scan(&created)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			created = time.Now().UTC()
		case err != nil:
			return fmt.Errorf("postgres: lookup api key: %w", err)
		}
	}
	settingsJSON, err := json.Marshal(k.Settings)
	if err != nil {
		return fmt.Errorf("postgres: marshal api key settings: %w", err)
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO api_keys (name, key_hash, home_ns, default_ns, created_at, disabled, settings, admin, read_only)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9)
		ON CONFLICT (name) DO UPDATE SET
			key_hash=EXCLUDED.key_hash, home_ns=EXCLUDED.home_ns, default_ns=EXCLUDED.default_ns,
			created_at=EXCLUDED.created_at, disabled=EXCLUDED.disabled, settings=EXCLUDED.settings, admin=EXCLUDED.admin,
			read_only=EXCLUDED.read_only`,
		k.Name, k.Hash, k.HomeNS, k.DefaultNS, created, k.Disabled, string(settingsJSON), k.Admin, k.ReadOnly)
	if err != nil {
		return fmt.Errorf("postgres: put api key: %w", err)
	}
	return tx.Commit(ctx)
}

// DeleteAPIKey removes the key by name. The bool reports whether a key
// existed to delete.
func (s *Store) DeleteAPIKey(ctx context.Context, name string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM api_keys WHERE name=$1`, name)
	if err != nil {
		return false, fmt.Errorf("postgres: delete api key: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListAPIKeys returns every key ordered by name.
func (s *Store) ListAPIKeys(ctx context.Context) ([]store.APIKey, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT name, key_hash, home_ns, default_ns, created_at, disabled, settings, admin, read_only FROM api_keys ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list api keys: %w", err)
	}
	return scanAPIKeys(rows)
}

// GetAPIKeyByHash returns the key whose hash matches, or nil, nil when none
// does.
func (s *Store) GetAPIKeyByHash(ctx context.Context, hash string) (*store.APIKey, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT name, key_hash, home_ns, default_ns, created_at, disabled, settings, admin, read_only FROM api_keys WHERE key_hash=$1`, hash)
	k, err := scanAPIKey(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get api key by hash: %w", err)
	}
	return &k, nil
}

// RenameAPIKeyNamespaces rewrites every key whose home_ns or default_ns
// equals from to to instead — both columns in one statement, so a namespace
// move (maintenance.Move, alongside RenameLinkEndpoints) leaves neither
// binding dangling. Unlike RenameLinkEndpoints there is no collision
// handling: neither column is part of a key's identity, so a plain UPDATE
// suffices. A no-op when from == to.
func (s *Store) RenameAPIKeyNamespaces(ctx context.Context, from, to string) error {
	if from == to {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE api_keys SET
			home_ns    = CASE WHEN home_ns    = $1 THEN $2 ELSE home_ns END,
			default_ns = CASE WHEN default_ns = $1 THEN $2 ELSE default_ns END
		WHERE home_ns = $1 OR default_ns = $1`, from, to)
	if err != nil {
		return fmt.Errorf("postgres: rename api key namespaces: %w", err)
	}
	return nil
}

// Ping verifies the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Close releases the connection pool.
func (s *Store) Close() error { s.pool.Close(); return nil }

// queryScoredChunk is queryScored for the chunk search, whose rows carry the
// matched chunk's text after the distance so the reranker can judge the passage
// that actually matched rather than the memory's prefix.
func (s *Store) queryScoredChunk(ctx context.Context, q string, vals []any, score func(float64) float64) ([]store.Scored, error) {
	rows, err := s.pool.Query(ctx, q, vals...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.Scored
	for rows.Next() {
		var (
			metric float64
			text   string
		)
		m, err := scanRow(rows, &metric, &text)
		if err != nil {
			return nil, err
		}
		out = append(out, store.Scored{Memory: m, Score: score(metric), MatchedChunk: text})
	}
	return out, rows.Err()
}

func (s *Store) queryScored(ctx context.Context, q string, vals []any, score func(float64) float64) ([]store.Scored, error) {
	rows, err := s.pool.Query(ctx, q, vals...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.Scored
	for rows.Next() {
		var metric float64
		m, err := scanMemoryWith(rows, &metric)
		if err != nil {
			return nil, err
		}
		out = append(out, store.Scored{Memory: m, Score: score(metric)})
	}
	return out, rows.Err()
}
