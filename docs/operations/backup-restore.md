# Backup and restore

What to copy, how to copy it while the server is running, and the two
invariants a restore must respect before the server will boot against it.

Two complementary paths:

- **Physical backups** (file copy, `pg_dump`, PVC snapshot): fast, complete,
  but tied to the backend and to the embedding configuration the store was
  built with.
- **Logical backups** (`memini export` / `memini import`): portable JSON that
  moves between backends and embedding models, because it carries text and
  metadata, not vectors — the target re-embeds. Slower, but immune to the
  restore invariants below.

Take both: physical for disaster recovery, a periodic export for portability
insurance.

## SQLite

The server runs SQLite in **WAL mode**. A live database is up to three files:

| File            | Holds                                       |
| --------------- | ------------------------------------------- |
| `memini.db`     | The main database                           |
| `memini.db-wal` | Committed transactions not yet checkpointed |
| `memini.db-shm` | Shared-memory index for the WAL             |

Copying only `memini.db` out from under a running server **loses every commit
still in the WAL**, and a copy taken mid-write can be internally inconsistent.
Either stop the server, or take a hot copy properly.

**Cold copy (simplest):** stop the server; a clean shutdown checkpoints the
WAL. Then copy the files — all of them, in case sidecars remain:

```sh
systemctl stop memini    # or scale the pod to 0
cp /var/lib/memini/memini.db* /backups/
systemctl start memini
```

**Hot copy (no downtime):** `VACUUM INTO` writes a transactionally consistent,
compacted, single-file snapshot while the server keeps running — WAL mode
allows concurrent readers, and the snapshot needs no `-wal`/`-shm` sidecars:

```sh
sqlite3 /data/memini.db "VACUUM INTO '/backups/memini-2026-08-17.db'"
```

The container image is distroless and ships no `sqlite3` binary; run this from
the host against the volume path, from a sidecar, or from any machine that can
see the file. Remember the volume runs as uid `65532` — the destination
directory must be writable by whoever runs the command.

**Restore:** stop the server, put the backup file at `MEMINI_SQLITE_PATH`,
delete any stale `-wal`/`-shm` files sitting next to it (they belong to the
old database), fix ownership (uid `65532` in the container), start.

## Postgres

Standard tooling applies:

```sh
pg_dump -Fc "$MEMINI_POSTGRES_DSN" -f memini.dump

pg_restore --clean --if-exists -d "$MEMINI_POSTGRES_DSN" memini.dump
```

Two Postgres-specific notes:

- **The target needs VectorChord.** The schema uses the `vchord` extension and
  `vchordrq` vector indexes. Restore into a server built from the same image
  family (`ghcr.io/tensorchord/vchord-postgres`) or one with VectorChord
  installed; a plain-pgvector server cannot take the restore.
- **Index rebuild dominates restore time.** The dump carries the vector data;
  `pg_restore` rebuilds the `vchordrq` indexes from it, and on a large corpus
  that build is the slow step. Budget for it, and do not kill a restore that
  looks stalled at the index stage.

## Kubernetes

The chart's sqlite backend keeps everything on one PVC mounted at `/data`, so
the backup unit is the PVC.

- **PVC snapshots** (a `VolumeSnapshotClass`, or VolSync replication) are the
  low-effort path. A snapshot of a live WAL database is crash-consistent —
  SQLite replays the WAL on open — but the clean ceremony is: scale the
  StatefulSet to 0, snapshot, scale back up. For zero-downtime, run
  `VACUUM INTO` to a second path on the same PVC and snapshot that.
- **Restoring**: create a PVC from the snapshot and point the chart at it with
  `persistence.data.existingClaim` (see
  [`values.yaml`](../../charts/memini/values.yaml)) instead of letting it
  provision a fresh one.
- **The fsck CronJob is not a backup tool.** It POSTs `/v1/fsck` — decay and
  hygiene maintenance — and it _mutates_ the store. While restoring or
  rolling back, suspend it so a maintenance pass does not sweep a
  half-restored store:
  `kubectl patch cronjob memini-fsck -p '{"spec":{"suspend":true}}'`.

With the Postgres backend, memini's pods are stateless; back up the database
(above) and nothing else.

## The portable path: export and import

`memini export` writes backend-neutral JSON (memories and links — no vectors);
`memini import` loads it into any backend, re-embedding as it goes. This is
the path that survives a backend switch, an embedding-model change, or a
dimensionality change:

```sh
memini export --all-namespaces -o backup.json
```

Flags, sources, the `--remote` mode for servers you cannot stop, and the
single-writer caveat (direct-store commands contend with a running sqlite
server) are all in [import-export.md](import-export.md).

## Restore invariants

A physical restore is only half done when the files are in place. The server
checks two things about the store it opens, and one of them cannot be fixed in
place.

**The store records its embedding model.** Booting against a restored store
whose recorded model differs from `MEMINI_EMBED_MODEL` **refuses to start**:
vectors from different models are not comparable, and a silent swap would
degrade recall with no error. The boot message names the three ways out:

1. Set `MEMINI_EMBED_MODEL` to match the restored data — the usual fix after a
   restore, since the mismatch means your config drifted, not the data.
2. Run `memini reembed` to rewrite every vector under the new model.
3. Set `MEMINI_REEMBED_ON_MODEL_CHANGE=true` to re-embed automatically at
   startup.

**Dimensionality can never be migrated in place.** The store's schema is fixed
at the `MEMINI_EMBED_DIMS` it was created with; `memini reembed` deliberately
keeps that width. Restoring a 384-dim store into a deployment configured for a
1536-dim model is not a reembed job — it needs a fresh store:
`memini export` from the backup, then `memini import` into a new store built
at the new width. See
[import-export.md](import-export.md#memini-reembed) for the full model-change
runbook.
