# Quickstart (with a disposable demo database)

This walkthrough uses only synthetic data and resources you create yourself. The same
seed → backup → verify flow was executed while preparing M0; the output below is abbreviated from that run.

## 1. Start a demo source database
```bash
openssl rand -hex 16 > demo.pass && chmod 600 demo.pass
```

```bash
docker run -d --name unth-demo-src -e POSTGRES_PASSWORD="$(cat demo.pass)" -e POSTGRES_DB=shop -p 127.0.0.1:55432:5432 postgres:16
```

```bash
PGPASSWORD="$(cat demo.pass)" psql -h 127.0.0.1 -p 55432 -U postgres -d shop -f tests/fixtures/seed.sql
```

## 2. Write a job configuration
```yaml
# demo.yaml
version: 1
source:
  name: shop-db
  host: 127.0.0.1
  port: 55432
  database: shop
  user: postgres
  password_file: ./demo.pass
verification:
  queries:
    - name: paid_order_total
      sql: SELECT sum(total_cents) FROM orders WHERE status = 'paid'
```

## 3. Check, back up, verify
```bash
unth doctor
unth discover postgres --config demo.yaml
unth backup postgres --config demo.yaml
```

```
Backup created: bk-20261109t183320-9da83155
  size:    511331 bytes
  status:  BACKUP_CREATED (not yet verified)
```

```bash
unth verify bk-20261109t183320-9da83155
```

```
  [PASS] artifact_accessible … [PASS] restore (216ms) … [PASS] assertion_queries
Final status:      VERIFIED
Data checks: 7 tables (7 content-hashed), rows expected=46800 actual=46800, 1 assertion queries
Cleanup: PASS (container:…, volume:unth-vol-…, network:unth-net-…)
```

## 4. Reports and history
```bash
unth backup list
unth report bk-20261109t183320-9da83155 --format json
unth audit list
```

## 5. See a failure
Corrupt the artifact and verify again — the result is `FAILED` / `CHECKSUM_MISMATCH`, exit code 3,
and no restore is attempted:
```bash
printf 'x' | dd of="$HOME/.local/share/unthrecover/backups/<id>/artifact.dump" bs=1 seek=1000 conv=notrunc
```

```bash
unth verify <id>; echo "exit=$?"
```

## 6. Remove the demo source
```bash
docker rm -f -v unth-demo-src
```
