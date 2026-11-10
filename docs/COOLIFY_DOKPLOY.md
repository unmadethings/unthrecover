# Coolify, Dokploy and Docker Compose

Unth Recover runs next to platforms that manage PostgreSQL in Docker. It never changes the
platform's containers: it connects to the database like any client, restores into its own
isolated test containers, and removes only what it created.

## 1. Find the databases

```bash
unth discover jobs                       # read-only; prints one job per PostgreSQL container
unth discover jobs --write-dir ./jobs    # writes jobs/<workload>.yaml (never overwrites)
```

For each container it recognizes the platform (Coolify / Dokploy from labels and networks,
Docker Compose from its labels) and fills in:

| Field | Source |
|---|---|
| `host` | container name (Coolify), swarm service name (Dokploy), service name (Compose) |
| `database`, `user` | `POSTGRES_DB` / `POSTGRES_USER` of the container |
| `password_env` | a variable name only — **the password is never read** from the container |
| `verification.restore_image` | the same image as the source, so extensions (PostGIS, TimescaleDB, pgvector) exist in the restore test |
| network note | the Docker network unth must join (`coolify`, `dokploy-network`, `<project>_default`) |

Where to find the password: Coolify → database → General → Password; Dokploy → database →
General → Database Password; Compose → `POSTGRES_PASSWORD` in the compose file.

## 2. Run unth on that network

Use the [Compose file](../deployments/compose/compose.yaml) from the release (or deploy it as a
Docker Compose resource in Coolify/Dokploy), put the generated jobs in `./jobs`, the passwords in
`.env`, and attach the scheduler to the platform's network:

```yaml
services:
  scheduler:
    networks: [default, platform]
networks:
  platform:
    name: coolify            # or dokploy-network, or myapp_default
    external: true
```

`TestDiscoverJobsCoolify` covers this path: a database created with Coolify's labels on its own
network is discovered, and the generated job, unchanged, is run by the unth container on that
network → backup VERIFIED, password never printed.

## 3. Offsite

Add `encryption`, `signing` and `offsite` to the jobs to keep encrypted copies on Google Cloud
Storage, S3 or Google Drive — see [OFFSITE.md](OFFSITE.md).

Not tested against live Coolify/Dokploy installations by the project yet: detection relies on
their documented labels/networks (`coolify.*`, `dokploy-network`, swarm service labels). Reports
welcome.
