# Operations

Running memini in anger: deploying it, upgrading it, and moving data through it.

| Doc                                    | Read it when                                                                                                                                                         |
| -------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [upgrading.md](upgrading.md)           | **The server refuses to boot.** Also: which settings were removed, which silently stopped applying, and which behaviour changed without a setting to change with it. |
| [deployment.md](deployment.md)         | You are standing memini up: prebuilt images and charts, Compose, a single container, Kubernetes, or bare metal under systemd.                                        |
| [production.md](production.md)         | You are putting it in front of a team: TLS and reverse proxies, resource sizing, Postgres operations, key rotation, and what `/metrics` exposes where.               |
| [backup-restore.md](backup-restore.md) | You want the data to survive: backing up SQLite and Postgres, PVC snapshots, the portable export path, and why a restore can refuse to boot.                         |
| [web-ui.md](web-ui.md)                 | You want the admin UI, or you want to know why exposing it hands out your API key.                                                                                   |
| [import-export.md](import-export.md)   | Loading memories in from another system, backing them up, switching embedding models, or undoing a bad import.                                                       |

Related, elsewhere:

- [reference/configuration.md](../reference/configuration.md) is the generated list of every environment variable, and of every one that was removed.
- [scopes.md](../scopes.md) is what a namespace can actually see, which is the thing operators get wrong most often.
