# EdgeVault

A production-inspired, hybrid-cloud file store built in **Go**. Files are split into chunks, compressed, encrypted, and backed by S3, with an edge node serving a local disk cache, crash-resilient write journal, ransomware protection, and immutable point-in-time versioning.

New volumes use 4 MiB AES-256-GCM encrypted chunks with KMS-wrapped data keys. Ransomware alerts save the clean manifest into the Object Lock snapshots bucket with seven-day per-object GOVERNANCE retention.

## Features

- **File dashboard:** create volumes, upload and download files, browse retained versions, and recover snapshots.
- **Storage pipeline:** fixed-size chunking, compression, encrypted chunks, deduplication, and S3-compatible object storage.
- **Edge resilience:** local disk cache, crash-resilient write journal, and background uploads.
- **Concurrent writes:** file leases, fencing, and conditional version checks.
- **Recovery:** ransomware alert handling, protected snapshots, retention policies, and garbage collection.
- **Deployment:** Docker Compose for local development and Terraform/Kubernetes manifests for AWS.

## Documentation

- [Local setup](#run-the-dashboard-locally)
- [Architecture](docs/architecture.md)
- [Dashboard behavior and limitations](web/README.md)
- [Build and test](#build-and-test)
- [AWS / Kubernetes deployment](#aws--kubernetes-deployment)
- [MIT License](LICENSE)

## Run the dashboard locally

### Prerequisites

- Go matching `go.mod` (currently Go 1.26.5).
- Docker Desktop, running, with Docker Compose available.
- `make`.
- Node.js and npm only if you want the optional Vite development workflow. Vite requires Node.js 20.19+ or 22.12+.

Run all commands below from the repository root unless noted otherwise.

### 1. Start PostgreSQL and local file storage

```sh
make up
```

This starts PostgreSQL on port **5433**, Silo (a MinIO-compatible S3 server) on port **9000**, and initializes the chunk and protected snapshot buckets. The Silo console is available at [http://localhost:9001](http://localhost:9001). Local development credentials are configured in the Makefile and Docker Compose file.

**This setup stores data locally, not in Amazon S3.** PostgreSQL and Silo keep their data in Docker volumes. `make down` stops the services without deleting those volumes; deleting the volumes removes their data. Starting only PostgreSQL is sufficient for the database-backed tests with in-memory blobs, but the normal application also needs Silo.

### 2. Start the Go API and dashboard

In another terminal, from the repository root:

```sh
make run
```

The Makefile supplies the local database, storage, and development key settings. The Go service applies database migrations and serves both the API and the embedded dashboard.

Open **[http://localhost:8080](http://localhost:8080)** and keep `make run` running while using the UI. No separate Node.js backend or frontend build is required for this workflow.

### 3. Use files and versions

1. Click **New volume**, enter a name, and create it.
2. Click **Upload files**, or drop files onto the file table.
3. Choose a destination path and upload. Reusing an existing path prompts before creating a new version; identical content leaves the version unchanged.
4. Use **Download** for the current file, or **History** to download a retained earlier version.
5. After deleting a file, enable **Show deleted files** to access its retained history.

To make an older downloaded version current again, upload it at the original path.

### 4. Create a snapshot and restore it

1. Select the volume and open **Snapshots & recovery**.
2. Click **Create snapshot**, enter a reason, and choose whether to request protection.
3. Make changes to the files.
4. Click **Restore** beside the desired snapshot.
5. Type the volume name and click **Restore volume**.

A restore applies to the **whole volume**. It creates new versions and removes current files that did not exist at the snapshot's commit. Protected snapshots request seven-day Object Lock retention and require the protected snapshot bucket initialized by `make up`. A standard snapshot records a recovery point without Object Lock protection.

## Frontend development and implementation

The UI uses **HTML, CSS, and JavaScript**, with the existing **Go API** as its backend:

```text
Browser → Go API → PostgreSQL (metadata and history)
                → Silo locally / Amazon S3 when deployed (file chunks)
```

| File | Purpose |
| --- | --- |
| `web/index.html` | Dashboard structure, file explorer, recovery panel, and dialogs |
| `web/style.css` | Responsive layout and styling |
| `web/app.js` | API requests, uploads, leases, history, snapshots, and restore controls |
| `web/web.go` | Embeds and serves the frontend assets from the Go binary |
| `internal/api/dashboard.go` | Read endpoints for volume summaries and dashboard data |
| `internal/meta/dashboard.go` | Consistent database reads for files, totals, and recent snapshots |
| `web/vite.config.js` | Development proxy to the Go service on port 8080 |

Uploads and deletes acquire a write lease before changing a file. Uploads also send the version observed by the UI using `If-Match`, preserving the backend's concurrency checks. The dashboard adds `GET /v1/volumes` and `GET /v1/volumes/{volume}/dashboard`; writes reuse the existing API.

### Optional: edit with Vite

Keep `make run` running on port **8080**. In a separate terminal:

```sh
cd web
npm ci
npm run dev
```

Open the URL printed by Vite, normally **http://127.0.0.1:5173**. Vite reloads frontend changes and proxies API, health, and metrics requests to Go on port 8080. If the port is occupied, Vite may choose another port; use the URL it prints.

To check the frontend build:

```sh
npm run build
```

This produces `web/dist`. Normal Go builds embed the source HTML/CSS/JS directly, so `web/dist` is optional. After editing the assets, restart `make run` to update the embedded UI on port 8080. Use `make build` from the repository root to compile the Go binaries into `bin/`.

### Ports and troubleshooting

| Address | Purpose |
| --- | --- |
| `http://localhost:8080` | Normal Go API and embedded dashboard; requires `make run` |
| Vite's printed URL, usually `http://127.0.0.1:5173` | Frontend development; requires both `npm run dev` and `make run` |
| `http://localhost:9001` | Local Silo object storage console |

- **404 on `/v1/volumes` at port 5173:** a plain static server is serving the frontend without the API proxy. Use port 8080, or stop that static server and launch `npm run dev` from `web/`. A command such as `python3 -m http.server` does not connect the frontend to Go. `npm run preview` serves the optional built assets; it is not the documented API-proxied development setup.
- **API proxy connection refused:** start `make run` and check its startup logs. The Vite proxy expects HTTP on port 8080; adjust `web/vite.config.js` if using another port or HTTPS.
- **Docker daemon unavailable:** open Docker Desktop, wait until it is ready, and retry `make up`.
- **Database or object storage unavailable:** inspect `docker compose -f deploy/docker/docker-compose.yml ps` and the container logs. Both PostgreSQL and Silo must be healthy for normal file operations.
- **Missing environment variables with `go run ./cmd/edge`:** use `make run`, which supplies the local development settings.
- **Writes are paused:** check the volume's ransomware alert and restore a clean snapshot through **Snapshots & recovery**.

The console uses the existing unauthenticated file/volume/snapshot API. Keep the development service on a trusted local/private network until authentication and authorization are implemented. See [web/README.md](web/README.md) for metric definitions and upload/download limitations.

## Repository layout

```text
cmd/             Edge service and garbage collector entry points
internal/        Storage, encryption, metadata, API, and recovery logic
web/             Embedded dashboard and optional Vite tooling
db/migrations/   PostgreSQL schema migrations
deploy/          Docker, Kubernetes, and Terraform configuration
docs/            Architecture documentation
bench/           End-to-end workload and recovery benchmark
```

Local planning notes, dependency directories, build output, and environment files containing secrets are excluded by `.gitignore`.

## Build and test

From the repository root:

```sh
# Compile both Go binaries, including the embedded dashboard
make build

# Storage engine unit tests
make test-unit

# Static analysis
make lint

# Embedded frontend asset and routing checks
go test ./web

# PostgreSQL-backed API tests, including the dashboard read endpoints.
# Uses the separate test database created by make up; the suite resets its schema.
EDGEVAULT_TEST_DB_URL="postgres://edgevault:edgevault@localhost:5433/edgevault_test" \
EDGEVAULT_CHUNK_HMAC_KEY="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" \
make test-integration
```

API integration tests skip when `EDGEVAULT_TEST_DB_URL` is unset. Use a dedicated test database: the suite resets its schema.

## AWS / Kubernetes deployment

The deployment files provision a VPC, private PostgreSQL, encrypted/versioned S3 buckets (the snapshot bucket has Object Lock), KMS, ECR, EKS, and narrowly scoped IRSA roles. The cluster runs two edge replicas with one persistent cache volume per pod. Use a private CA certificate whose SAN covers `edgevault.edgevault.svc.cluster.local`; do not deploy the sample with a self-signed production cert.

Terraform is pinned to AWS provider 6.x and currently targets EKS Kubernetes 1.37. Before applying, set `TF_VAR_cluster_admin_cidrs` to your workstation's public IPv4 `/32` and review the plan. `db_instance_class` and `node_instance_types` are variables for cost/availability tradeoffs.

```bash
cd deploy/terraform
terraform init
terraform validate
terraform plan -out=edgevault.tfplan
terraform apply edgevault.tfplan
cd ../..

export AWS_REGION="$(terraform -chdir=deploy/terraform output -raw aws_region)"
export ECR_URL="$(terraform -chdir=deploy/terraform output -raw ecr_repository_url)"
export EDGEVAULT_IMAGE="$ECR_URL:$(date +%Y%m%d%H%M%S)"
aws ecr get-login-password --region "$AWS_REGION" | docker login --username AWS --password-stdin "$ECR_URL"
docker buildx build --platform linux/amd64 -f deploy/docker/Dockerfile -t "$EDGEVAULT_IMAGE" --push .

aws eks update-kubeconfig --region "$AWS_REGION" --name "$(terraform -chdir=deploy/terraform output -raw eks_cluster_name)"
kubectl apply -f deploy/k8s/base/namespace.yaml
python3 deploy/k8s/make-runtime-secret.py | kubectl apply -f -
kubectl -n edgevault create secret tls edgevault-tls --cert=./tls.crt --key=./tls.key --dry-run=client -o yaml | kubectl apply -f -
EDGEVAULT_IMAGE="$EDGEVAULT_IMAGE" python3 deploy/k8s/render.py | kubectl apply -f -
kubectl -n edgevault rollout status statefulset/edgevault
```

`make-runtime-secret.py` reads the RDS-managed password without printing it and preserves the existing HMAC/admin tokens when rerun. It requires AWS CLI credentials, `terraform`, `kubectl`, and Python 3. To reach the HTTPS-only service locally, use `kubectl -n edgevault port-forward service/edgevault 8443:8443` and call it with the issuing CA. Prometheus must trust that CA when scraping `/metrics`.

The CronJob starts with `--dry-run`. Confirm its logs and the GC test results before removing that argument. The Edge IRSA policy has no `s3:DeleteObject`; only the GC role has delete permission, limited to chunk objects. The deployment acceptance test from an edge pod is `aws s3 rm "s3://$(terraform -chdir=deploy/terraform output -raw chunks_bucket)/some-key"` and it must be denied. Run the Phase 5 concurrent-writer integration test against the service twice while both replicas are ready.

The local MinIO Object Lock check confirmed that protected snapshot objects carry GOVERNANCE retention metadata and that a version-specific delete is refused until expiry. In a versioned S3 bucket, an ordinary `DeleteObject` can add a delete marker while leaving the retained version intact; the Edge IRSA role therefore has no delete permission on the snapshot bucket either.

## Retention and garbage collection

Retention is configured per volume through `PUT /v1/volumes/{volume}/retention`, authenticated with `Authorization: Bearer $EDGEVAULT_ADMIN_TOKEN`. The JSON body accepts `keep_days`, `min_versions`, and `floor_days`; `floor_days` cannot be less than seven, and the other values cannot undercut it. Policy changes are audited. GC first expires eligible version manifests, preserving the current version, the configured number of newest versions, versions within the retention window, and versions visible in protected snapshots. It then finds unreferenced chunks after a 24-hour grace period and deletes them from S3 before removing their metadata row. An active ransomware alert suspends cleanup for that volume.

Preview a pass with `make gc-dry-run`; run a live pass with `go run ./cmd/gc` only after verifying the dry-run report and the GC identity's bucket scope. The Kubernetes CronJob is intentionally configured for dry-run by default.

These resources incur AWS charges. When finished with a deployment, run `terraform -chdir=deploy/terraform destroy` and confirm the plan before applying it.

## Measurements

Run the workload against a new volume name: `go run ./bench -url http://localhost:8080 -volume bench-$(date +%s) -files 100 -size-bytes 65536 -workers 8`. For the Kubernetes deployment, use `https://edgevault.edgevault.svc.cluster.local:8443` from a pod that trusts the configured CA. The benchmark verifies reads, simulates a high-entropy attack against clean baseline files, times the alert and restore, and byte-checks restored content. It reports measured throughput, read/write latency, cache hit ratio, and dedup ratio; it does not invent target values.

| Environment | Files / bytes | Workers | Throughput MiB/s | Mean/max PUT ms | Mean/max GET ms | Cache hit ratio | Dedup ratio | Detection / restore time |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| Local (single run, Docker PostgreSQL + MinIO, one edge) | 50 / 3.125 MiB | 8 | 3.13 | 147.19 / 225.62 | 9.38 / 35.07 | 1.00 | 0.0357 | 86.49 / 34.83 |

The local row was measured on 2026-10-29 with `-files 50 -size-bytes 65536 -workers 8`; values are workload-specific and are not an EKS performance claim. Detection/restore values are milliseconds.

## License

Licensed under the [MIT License](LICENSE).
