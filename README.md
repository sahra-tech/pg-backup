# PostgreSQL Backup Application

A Go service that backs up PostgreSQL databases on a schedule, compresses them, ships them to local
disk or S3, and prunes them when they expire.

## Features

- **Flexible Database Selection**: Back up specific databases, or discover and back up all of them
- **Full Dump Mode**: A single `pg_dumpall` file containing every database, role, and tablespace
- **Multiple Storage Options**: Local filesystem or S3 (including S3-compatible endpoints), via AWS SDK for Go v2
- **Scheduled Backups**: Cron-based scheduling, validated at startup
- **Streaming Compression**: gzip applied as the dump streams, so memory stays flat on large clusters
- **Retention**: Expired backups are pruned automatically after each successful run
- **Health Monitoring**: HTTP endpoints for health, status, and manual triggering
- **Atomic Writes**: An interrupted or failed dump never leaves a partial file behind
- **Graceful Shutdown**: SIGTERM stops the scheduler and cleans up in-flight work
- **Docker Support**: Non-root image with a built-in healthcheck

## Quick Start

1. **Build:**

   ```bash
   make build
   ```

2. **Configure:**

   ```bash
   cp .env.example .env
   ```

3. **Run a test backup:**

   ```bash
   ./pg-backup --env-file .env -once
   ```

4. **Start the scheduler:**

   ```bash
   ./pg-backup --env-file .env
   ```

## Configuration

All settings come from the environment. Standard PostgreSQL (`PG*`) and AWS (`AWS_*`) variables are
used where they exist; everything else is prefixed `PG_BACKUP_`.

Copy [.env.example](.env.example) and edit it:

```bash
cp .env.example .env
```

Then supply it however suits the deployment:

```bash
docker compose up -d              # compose reads .env via env_file
./pg-backup --env-file .env       # ad-hoc CLI runs
set -a && source .env && set +a   # plain shell
```

Variables already present in the environment always win over `--env-file`, so an orchestrator or an
operator can override a checked-in default without editing the file.

Every problem is reported at once, so a mistyped deployment learns about all of its errors in a
single run rather than one restart at a time.

### Reference

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `PGHOST` | yes | — | Database host |
| `PGUSER` | yes | — | Database user |
| `PGPORT` | no | `5432` | Database port |
| `PGPASSWORD` | no | — | Database password |
| `PGSSLMODE` | no | `prefer` | `disable`\|`allow`\|`prefer`\|`require`\|`verify-ca`\|`verify-full` |
| `PG_BACKUP_DATABASES` | no | all | Comma-separated list; empty discovers every database |
| `PG_BACKUP_STORAGE_TYPE` | yes | — | `local` or `s3` |
| `PG_BACKUP_LOCAL_PATH` | if local | — | Directory for backups |
| `PG_BACKUP_S3_BUCKET` | if s3 | — | Bucket name |
| `AWS_REGION` | no | `us-east-1` | Region |
| `AWS_ACCESS_KEY_ID` | if s3 | — | Access key |
| `AWS_SECRET_ACCESS_KEY` | if s3 | — | Secret key |
| `PG_BACKUP_S3_ENDPOINT` | no | AWS | Custom endpoint; bare host is fine |
| `PG_BACKUP_SCHEDULE` | yes | — | Cron, or `@daily` / `@every 6h` |
| `PG_BACKUP_LOG_FILE` | no | — | Extra log file; stderr always receives logs |
| `PG_BACKUP_RUN_ON_START` | no | `false` | Back up immediately at startup |
| `PG_BACKUP_RETENTION_DAYS` | no | `30` | Prune older backups; `0` disables |
| `PG_BACKUP_FULL_DUMP` | no | `false` | Single `pg_dumpall` file for the cluster |
| `PG_BACKUP_COMPRESSION_LEVEL` | no | `-1` | `-1` default, `0` none, `1`–`9` |
| `PG_BACKUP_HEALTH_PORT` | no | `8080` | Health server port |
| `PG_BACKUP_HEALTH_BIND` | no | all | `127.0.0.1` restricts to loopback |
| `PG_BACKUP_TRIGGER_TOKEN` | no | — | Bearer token for `POST /trigger` |

`PG_BACKUP_DATABASES` is comma-separated, so a database whose name contains a comma cannot be listed
there; leave the variable empty to have such a database discovered automatically.

## Commands

- `./pg-backup` — start the scheduler
- `./pg-backup -once` — run one backup and exit (exit code 1 if anything failed)
- `./pg-backup -list` — list configured databases
- `./pg-backup --env-file .env` — load a `KEY=value` file before reading configuration

## Backup Modes

### Individual Database Backups (`full_dump: false`)

Runs `pg_dump` per database and writes one `.sql.gz` each, which allows selective restores. If one
database fails the others still run; the process reports which ones failed and exits non-zero.

### Full Dump Mode (`full_dump: true`)

Runs `pg_dumpall` once and writes a single `.sql.gz` containing every database plus roles,
tablespaces, and other global objects. Required for a complete cluster restore including
permissions. The `databases` list is ignored in this mode.

## S3 and S3-compatible storage

Uploads use multipart transfer, which means:

- No 5GB object-size limit.
- Memory stays bounded (~64MB of upload buffers) regardless of dump size.
- Each part is retried independently, so one flaky request does not restart the
  whole upload.

Transient gateway failures (HTTP 500/502/503/504) are retried up to 6 times, above the SDK default
of 3, because CDN/proxy layers in front of S3-compatible stores return them far more often than AWS
S3 does. If the upload still fails, the incomplete multipart upload is aborted so no orphaned parts
are billed.

> If uploads are ever killed abruptly (SIGKILL, power loss), the abort cannot run. Configuring a
> bucket lifecycle rule to expire incomplete multipart uploads after a day or two is good practice.

`PG_BACKUP_S3_ENDPOINT` may be given as a bare host (`s3.example.com`) or with a scheme
(`https://s3.example.com`). Bucket addressing is virtual-host style, matching the previous
behaviour.

### Diagnosing upload failures

A gateway in front of the object store answers outages with an HTML error page rather than an S3
XML `<Error>`. Errors now name the HTTP status and the endpoint up front:

```
s3 upload of full_dump_....sql.gz failed: endpoint https://s3.example.com returned HTTP 502.
This is a gateway/upstream failure at the storage provider, not a credentials problem;
it was retried 6 times: ...
```

An HTTP 5xx here is the provider's problem, not a misconfiguration on your side. Persistent 502s
usually mean the storage endpoint or the CDN in front of it is unhealthy.

## Retention

After every **successful** run, backups older than `retention_days` are deleted. Two guard rails
apply:

- The sweep is skipped entirely if any part of the run failed, so old copies are never removed when
  a fresh backup might be missing.
- Only files matching this tool's own naming pattern (`<name>_<timestamp>[_<suffix>].sql.gz`) are
  ever considered, so a shared bucket or directory is safe.

Set `PG_BACKUP_RETENTION_DAYS=0` to disable pruning.

## Health Monitoring

- `GET /health` — liveness check
- `GET /status` — last/next backup time, uptime, counters, whether a backup is running
- `POST /trigger` — start a backup immediately

```bash
curl http://localhost:8080/status
```

### Manual trigger

```bash
export PG_BACKUP_TRIGGER_TOKEN=your-token
./trigger-backup.sh
```

Or directly:

```bash
curl -X POST -H "Authorization: Bearer $PG_BACKUP_TRIGGER_TOKEN" http://localhost:8080/trigger
```

Responses: `202` accepted, `409` a backup is already running, `401` bad or missing token,
`405` wrong method.

> **Security:** `/trigger` starts real work against your database. Set `PG_BACKUP_TRIGGER_TOKEN`,
> and/or set `PG_BACKUP_HEALTH_BIND=127.0.0.1`. The shipped `docker-compose.yml` publishes the port on loopback
> only. The service warns at startup if no token is configured.

Only one backup runs at a time; overlapping schedules and triggers are rejected with `409` rather
than stacking concurrent dumps.

## Docker Deployment

```bash
make docker-build
make docker-run
```

Built on `golang:1.27-alpine`, running on `alpine:3.24`. The image runs as an unprivileged user
(uid 65532) and includes a `HEALTHCHECK` against `/health`.
Mounted `backups/` and `logs/` directories must be writable by that uid.

### Logs

Logs always go to **stderr**, so they appear in `docker logs pg-backup`, journald, or whatever your
supervisor collects — no configuration, no volume, and no `docker exec` needed.

`PG_BACKUP_LOG_FILE` is optional and only adds a second destination. If you set it, the path must be
writable by uid 65532 (the container runs unprivileged); a bind mount keeps its host-side ownership,
which the image's own `chown` cannot change. If the file cannot be opened, pg-backup logs a warning
and carries on with stderr only — a log-file permission problem never stops a backup.

## Development

```bash
make check   # gofmt, go vet, go test -race
```

`go.mod` declares Go 1.24 as the **minimum**; CI and the Docker image build with 1.27.

## Troubleshooting

**Connection issues.** Verify host, port, user, and password. Check that the server accepts
connections from the backup host (`pg_hba.conf`, firewall). If the server requires TLS, set
`sslmode: require` or stricter.

**`pg_dump`/`pg_dumpall` not found.** Install the PostgreSQL client tools and ensure they are on
`PATH`:

```bash
sudo apt-get install postgresql-client   # Debian/Ubuntu
brew install libpq                       # macOS
```

The Docker image installs `postgresql17-client` and puts `/usr/libexec/postgresql` on `PATH`.

**Backups fail for one database but not others.** Each database is dumped independently. The error
log names the failing database and includes `pg_dump` stderr; the remaining databases still run.
