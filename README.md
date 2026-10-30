# EdgeVault

A production-inspired, hybrid-cloud file store built in **Go**. Files are split into chunks, compressed, encrypted, and backed by S3, with an edge node serving a local disk cache, crash-resilient write journal, ransomware protection, and immutable point-in-time versioning.

New volumes use 4 MiB AES-256-GCM encrypted chunks with KMS-wrapped data keys. Ransomware alerts save the clean manifest into the Object Lock snapshots bucket with seven-day per-object GOVERNANCE retention.

Built for interview defense and hands-on mastery of cloud storage architectures (AWS S3, KMS, RDS PostgreSQL, EKS, distributed caching, deduplication, and lease fencing).

---

## Quick Navigation

- **[CONTINUE.md](file:///Users/vedaswarupbhushan/Documents/projects/EdgeVault/CONTINUE.md)** — **Start here!** Full handover guide, current status, exact test/run commands, interview Q&A, and resume prompt.
- **[EdgeVault_AI_build_prompt.md](file:///Users/vedaswarupbhushan/Documents/projects/EdgeVault/EdgeVault_AI_build_prompt.md)** — Master engineering specification and phase requirements.

---

## Quick Start

### 1. Start Database
```bash
docker start edgevault-pg
# or
docker compose -f deploy/docker/docker-compose.pg-only.yml up -d
```

### 2. Run Tests
```bash
# Unit tests
make test-unit

# PostgreSQL-backed integration tests, including encryption, restore,
# optimistic concurrency, and simulated ransomware recovery:
EDGEVAULT_TEST_DB_URL="postgres://edgevault:edgevault@localhost:5433/edgevault_test" \
EDGEVAULT_CHUNK_HMAC_KEY="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" \
make test-integration
```

### 3. Build Binaries
```bash
make build
```

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

The local MinIO Object Lock check confirmed that protected snapshot objects carry GOVERNANCE retention metadata and that a version-specific delete is refused until expiry. In a versioned S3 bucket, an ordinary `DeleteObject` can add a delete marker while leaving the retained version intact; the Edge IRSA role therefore has no delete permission on the snapshot bucket either. Local acceptance runs left three protected test objects under `protected/live-object-lock/`, `protected/live-cross-edge-ransom/`, and `protected/bench-final-local-20261029/`; their seven-day retention ends on 2026-11-05 UTC.

## Retention and garbage collection

Retention is configured per volume through `PUT /v1/volumes/{volume}/retention`, authenticated with `Authorization: Bearer $EDGEVAULT_ADMIN_TOKEN`. The JSON body accepts `keep_days`, `min_versions`, and `floor_days`; `floor_days` cannot be less than seven, and the other values cannot undercut it. Policy changes are audited. GC first expires eligible version manifests, preserving the current version, the configured number of newest versions, versions within the retention window, and versions visible in protected snapshots. It then finds unreferenced chunks after a 24-hour grace period and deletes them from S3 before removing their metadata row. An active ransomware alert suspends cleanup for that volume.

Preview a pass with `make gc-dry-run`; run a live pass with `go run ./cmd/gc` only after verifying the dry-run report and the GC identity's bucket scope. The Kubernetes CronJob is intentionally configured for dry-run by default.

These resources incur AWS charges. When finished with a deployment, run `terraform -chdir=deploy/terraform destroy` and confirm the plan before applying it.

## Measurements

Run the workload against a new volume name: `go run ./bench -url http://localhost:8080 -volume bench-$(date +%s) -files 100 -size-bytes 65536 -workers 8`. For the Kubernetes deployment, use `https://edgevault.edgevault.svc.cluster.local:8443` from a pod that trusts the configured CA. The benchmark verifies reads, simulates a high-entropy attack against clean baseline files, times the alert and restore, and byte-checks restored content. It reports measured throughput, read/write latency, cache hit ratio, and dedup ratio; it does not invent target values.

| Environment | Files / bytes | Workers | Throughput MiB/s | Mean/max PUT ms | Mean/max GET ms | Cache hit ratio | Dedup ratio | Detection / restore time |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| Local (single run, Docker PostgreSQL + MinIO, one edge) | 50 / 3.125 MiB | 8 | 3.13 | 147.19 / 225.62 | 9.38 / 35.07 | 1.00 | 0.0357 | 86.49 / 34.83 |
| EKS (2 replicas) |  |  |  |  |  |  |  |  |

The local row was measured on 2026-10-29 with `-files 50 -size-bytes 65536 -workers 8`; values are workload-specific and are not an EKS performance claim. Detection/restore values are milliseconds.
