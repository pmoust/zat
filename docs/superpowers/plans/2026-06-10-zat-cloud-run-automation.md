# zat on Cloud Run Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deploy `zat` (Google Meet → Drive archival + Slack notifications) as a scheduled, config-driven Cloud Run Job in GCP project `nofire-production`, auto-deployed from the `nofireai/engineering` repo at near-zero cost.

**Architecture:** A Cloud Run **Job** runs `zat -no-server` every 2 hours via Cloud Scheduler, then exits. `zat.yml` and the three credential files live as Secret Manager secrets, mounted read-only and copied to a writable dir by an entrypoint wrapper. GitHub Actions in `nofireai/engineering` push `zat.yml` edits as new secret versions (fast path) and build/deploy infra via Terraform (infra path), authenticating to GCP with keyless Workload Identity Federation.

**Tech Stack:** Terraform (Google provider ~> 6.0), Docker (distroless static), Cloud Run Jobs v2, Cloud Scheduler, Secret Manager, Artifact Registry, GitHub Actions, Workload Identity Federation, Go 1.23 (build only).

---

## Implementation location

All implementation files are created in the **`nofireai/engineering`** repo, checked out locally at `/Users/pmoust/nofire/engineering`. The container source is built from `github.com/pmoust/zat` at a pinned ref.

This plan document and the design spec live in the **`pmoust/zat`** repo (`/Users/pmoust/nofire/zat/docs/superpowers/`).

## Two-phase deployment reality

- **Bootstrap (manual, one-time, by a project admin):** the first `terraform apply` creates the Workload Identity Federation pool, the deployer service account, and all IAM — so it must be run by a human with admin credentials, not by CI (chicken-and-egg). The three credential secrets are also loaded manually once.
- **Steady state (CI):** thereafter, `zat-config.yml` adds `zat.yml` secret versions and `zat-deploy.yml` runs `terraform apply` + image build, both authenticating via WIF as the deployer SA.

## File structure (`nofireai/engineering`)

```
zat/
  zat.yml                      # config engineers edit
  ZAT_REF                      # single source of truth for the pinned pmoust/zat git ref + image tag
  Dockerfile                   # multi-stage build of pmoust/zat @ ZAT_REF on distroless static
  entrypoint.sh                # copies /secrets/* → /tmp/config, exec zat
  .gitignore                   # ignore local terraform junk
  terraform/
    versions.tf                # terraform + provider version pins, GCS backend
    variables.tf               # project_id, region, image_tag, github_repo, state_bucket
    providers.tf               # google provider config
    artifact_registry.tf       # docker repo + cleanup policy
    secrets.tf                 # 4 secret containers
    service_accounts.tf        # runtime, scheduler, deployer SAs
    iam.tf                     # WIF pool/provider, project roles, secret accessors, SA bindings
    cloud_run_job.tf           # the zat job (secret volumes, args, runtime SA)
    scheduler.tf               # cron → jobs:run with OAuth
    outputs.tf                 # values needed for bootstrap & GitHub secrets
    terraform.tfvars.example   # documented example values
  README.md                    # bootstrap + how to edit zat.yml + cost notes
.github/workflows/             # repo root (GitHub only reads workflows here)
  zat-config.yml               # paths: zat/zat.yml → add secret version + prune
  zat-deploy.yml               # paths: zat/Dockerfile|entrypoint.sh|ZAT_REF|terraform/** → build + apply
```

## Verification tooling

Available locally: `terraform`, `shellcheck`, `actionlint`. Not installed: `tflint`, `hadolint`, and `docker` is only needed in CI/bootstrap (image builds clone the pinned ref from GitHub). Per-task verification uses `terraform fmt -check` + `terraform validate` (after `terraform init -backend=false`), `shellcheck`, and `actionlint`.

---

### Task 1: Scaffold the `zat/` directory and config files

**Files:**
- Create: `/Users/pmoust/nofire/engineering/zat/zat.yml`
- Create: `/Users/pmoust/nofire/engineering/zat/ZAT_REF`
- Create: `/Users/pmoust/nofire/engineering/zat/.gitignore`

- [ ] **Step 1: Create the directory**

```bash
mkdir -p /Users/pmoust/nofire/engineering/zat/terraform
```

- [ ] **Step 2: Create `zat/zat.yml`** (the four current directives — engineers edit this file going forward)

```yaml
# zat archival config. `name` is used in the archived folder/file names, so set
# it to whatever you want the recordings labeled as. `google` is the Drive
# folder ID, `meet` is the meeting code (the abc-defg-hij part of a
# meet.google.com link), `slack` is the channel ID to notify.
#
# Editing this file and merging to main pushes a new Secret Manager version;
# the change takes effect on the next 2-hourly run. See zat/README.md.
- name: eng-all-hands
  google: 1md47_0LNV3uMcG3W7v__huKxV58w14R0
  meet: wdz-hbbv-mxc
  slack: C087PMJ6ZHV

- name: orchestration-context
  google: 13zgBJoEypNrz4iCxMk2_vee73Bk9-uHX
  meet: fij-ibgs-wbv
  slack: C0B1FBT1URY

- name: demo
  google: 13zgBJoEypNrz4iCxMk2_vee73Bk9-uHX
  meet: pzs-jhwg-zuc
  slack: C0B1FBT1URY

- name: runtime-controls
  google: 1zeZn7a0olGn8HRG-lhA2dX_Pui_XIg4w
  meet: csq-phqy-acr
  slack: C0B1BV2266R
```

- [ ] **Step 3: Create `zat/ZAT_REF`** (the pinned source ref; also used as the image tag — replace with a real pushed commit SHA from `pmoust/zat` during bootstrap)

```
main
```

- [ ] **Step 4: Create `zat/.gitignore`**

```gitignore
# local terraform state & plan artifacts (state is in GCS)
terraform/.terraform/
terraform/*.tfstate
terraform/*.tfstate.*
terraform/tfplan
terraform/.terraform.lock.hcl
# never commit real credential files
*.creds.json
google.config.json
slack.config.json
```

- [ ] **Step 5: Commit**

```bash
cd /Users/pmoust/nofire/engineering
git add zat/zat.yml zat/ZAT_REF zat/.gitignore
git commit -m "zat: scaffold config dir, pinned ref, gitignore"
```

---

### Task 2: Entrypoint wrapper

**Files:**
- Create: `/Users/pmoust/nofire/engineering/zat/entrypoint.sh`

- [ ] **Step 1: Write `zat/entrypoint.sh`**

Secret Manager mounts are read-only, but `zat` writes `google.creds.json` back on every token refresh. The wrapper copies the four mounted secrets into writable `/tmp/config`, then execs zat there.

```bash
#!/bin/sh
# Copy read-only mounted secrets into a writable config dir, then run zat.
# zat rewrites google.creds.json on token refresh, which would fail against the
# read-only Secret Manager mount; /tmp/config is writable and ephemeral.
set -eu

CONFIG_DIR=/tmp/config
mkdir -p "$CONFIG_DIR"

cp /secrets/zat-config/zat.yml              "$CONFIG_DIR/zat.yml"
cp /secrets/google-config/google.config.json "$CONFIG_DIR/google.config.json"
cp /secrets/google-creds/google.creds.json   "$CONFIG_DIR/google.creds.json"
cp /secrets/slack-config/slack.config.json   "$CONFIG_DIR/slack.config.json"

exec /usr/local/bin/zat -config-dir "$CONFIG_DIR" "$@"
```

- [ ] **Step 2: Make it executable**

```bash
chmod +x /Users/pmoust/nofire/engineering/zat/entrypoint.sh
```

- [ ] **Step 3: Verify with shellcheck**

Run: `shellcheck /Users/pmoust/nofire/engineering/zat/entrypoint.sh`
Expected: no output (exit 0).

- [ ] **Step 4: Commit**

```bash
cd /Users/pmoust/nofire/engineering
git add zat/entrypoint.sh
git commit -m "zat: add entrypoint wrapper for read-only secret mounts"
```

---

### Task 3: Dockerfile

**Files:**
- Create: `/Users/pmoust/nofire/engineering/zat/Dockerfile`

- [ ] **Step 1: Write `zat/Dockerfile`**

Multi-stage: clone `pmoust/zat` at `ZAT_REF`, build a static binary (CGO off), copy onto distroless static with the entrypoint wrapper. `busybox` shell is needed for `entrypoint.sh`, so use `distroless/static` plus a copied `sh`? Distroless static has no shell — use `gcr.io/distroless/base` is also shell-less. Instead use a minimal `alpine` runtime (still tiny, ~8 MB) so `/bin/sh` exists for the wrapper.

```dockerfile
# syntax=docker/dockerfile:1

FROM golang:1.23-alpine AS build
ARG ZAT_REF=main
RUN apk add --no-cache git
WORKDIR /src
RUN git clone https://github.com/pmoust/zat.git . \
 && git checkout "${ZAT_REF}"
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/zat .

FROM alpine:3.20
RUN adduser -D -u 10001 zat
COPY --from=build /out/zat /usr/local/bin/zat
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
USER 10001
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
```

- [ ] **Step 2: Lint the Dockerfile syntax** (hadolint not installed; do a basic build-context sanity check instead)

Run: `grep -q "ARG ZAT_REF" /Users/pmoust/nofire/engineering/zat/Dockerfile && echo OK`
Expected: `OK`

- [ ] **Step 3: Commit**

```bash
cd /Users/pmoust/nofire/engineering
git add zat/Dockerfile
git commit -m "zat: add Dockerfile building pmoust/zat at pinned ref"
```

> Note: a real `docker build` happens in CI / bootstrap (Task 12 / Task 13) because it clones the pushed ref from GitHub. It is not part of per-task local verification.

---

### Task 4: Terraform — versions, providers, variables

**Files:**
- Create: `/Users/pmoust/nofire/engineering/zat/terraform/versions.tf`
- Create: `/Users/pmoust/nofire/engineering/zat/terraform/providers.tf`
- Create: `/Users/pmoust/nofire/engineering/zat/terraform/variables.tf`
- Create: `/Users/pmoust/nofire/engineering/zat/terraform/terraform.tfvars.example`

- [ ] **Step 1: Write `versions.tf`**

```hcl
terraform {
  required_version = ">= 1.6.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }

  backend "gcs" {
    # bucket is supplied at init time:
    #   terraform init -backend-config="bucket=<state_bucket>"
    prefix = "zat"
  }
}
```

- [ ] **Step 2: Write `providers.tf`**

```hcl
provider "google" {
  project = var.project_id
  region  = var.region
}
```

- [ ] **Step 3: Write `variables.tf`**

```hcl
variable "project_id" {
  type        = string
  description = "GCP project hosting zat."
  default     = "nofire-production"
}

variable "region" {
  type        = string
  description = "Region for Artifact Registry, Cloud Run Job, and Scheduler."
  default     = "us-central1"
}

variable "image_tag" {
  type        = string
  description = "Artifact Registry image tag to deploy (matches zat/ZAT_REF; a pmoust/zat git SHA or tag)."
}

variable "github_repo" {
  type        = string
  description = "owner/repo allowed to assume the deployer SA via Workload Identity Federation."
  default     = "nofireai/engineering"
}

variable "schedule" {
  type        = string
  description = "Cloud Scheduler cron for archival runs."
  default     = "0 */2 * * *"
}

variable "scheduler_time_zone" {
  type        = string
  description = "Time zone for the scheduler cron."
  default     = "Etc/UTC"
}
```

- [ ] **Step 4: Write `terraform.tfvars.example`**

```hcl
project_id = "nofire-production"
region     = "us-central1"
image_tag  = "REPLACE_WITH_PMOUST_ZAT_SHA"
# github_repo, schedule, scheduler_time_zone use defaults
```

- [ ] **Step 5: Verify terraform formatting and schema**

Run:
```bash
cd /Users/pmoust/nofire/engineering/zat/terraform
terraform fmt -check
terraform init -backend=false
terraform validate
```
Expected: `fmt` prints nothing; `init` succeeds downloading the google provider; `validate` reports `Success! The configuration is valid.` (a warning that `image_tag` has no value is fine — validate does not require variable values).

- [ ] **Step 6: Commit**

```bash
cd /Users/pmoust/nofire/engineering
git add zat/terraform/versions.tf zat/terraform/providers.tf zat/terraform/variables.tf zat/terraform/terraform.tfvars.example
git commit -m "zat: terraform skeleton (versions, provider, variables)"
```

---

### Task 5: Terraform — Artifact Registry

**Files:**
- Create: `/Users/pmoust/nofire/engineering/zat/terraform/artifact_registry.tf`

- [ ] **Step 1: Write `artifact_registry.tf`** (docker repo + cleanup policy to stay under the 0.5 GB free tier)

```hcl
resource "google_artifact_registry_repository" "zat" {
  location      = var.region
  repository_id = "zat"
  description   = "zat container images"
  format        = "DOCKER"

  cleanup_policies {
    id     = "keep-recent-3"
    action = "KEEP"
    most_recent_versions {
      keep_count = 3
    }
  }

  cleanup_policies {
    id     = "delete-old"
    action = "DELETE"
    condition {
      older_than = "2592000s" # 30 days
    }
  }
}

locals {
  image = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.zat.repository_id}/zat:${var.image_tag}"
}
```

- [ ] **Step 2: Verify**

Run:
```bash
cd /Users/pmoust/nofire/engineering/zat/terraform
terraform fmt -check && terraform validate
```
Expected: `Success! The configuration is valid.`

- [ ] **Step 3: Commit**

```bash
cd /Users/pmoust/nofire/engineering
git add zat/terraform/artifact_registry.tf
git commit -m "zat: terraform Artifact Registry repo + cleanup policy"
```

---

### Task 6: Terraform — Secret containers

**Files:**
- Create: `/Users/pmoust/nofire/engineering/zat/terraform/secrets.tf`

- [ ] **Step 1: Write `secrets.tf`** (Terraform manages the secret *containers*; versions are loaded by bootstrap and the config workflow)

```hcl
locals {
  secrets = {
    config        = "zat-config"        # zat.yml
    google_config = "zat-google-config" # google.config.json
    google_creds  = "zat-google-creds"  # google.creds.json
    slack_config  = "zat-slack-config"  # slack.config.json
  }
}

resource "google_secret_manager_secret" "zat" {
  for_each  = local.secrets
  secret_id = each.value

  replication {
    auto {}
  }
}
```

- [ ] **Step 2: Verify**

Run:
```bash
cd /Users/pmoust/nofire/engineering/zat/terraform
terraform fmt -check && terraform validate
```
Expected: `Success! The configuration is valid.`

- [ ] **Step 3: Commit**

```bash
cd /Users/pmoust/nofire/engineering
git add zat/terraform/secrets.tf
git commit -m "zat: terraform Secret Manager containers"
```

---

### Task 7: Terraform — Service accounts

**Files:**
- Create: `/Users/pmoust/nofire/engineering/zat/terraform/service_accounts.tf`

- [ ] **Step 1: Write `service_accounts.tf`**

```hcl
resource "google_service_account" "runtime" {
  account_id   = "zat-runtime"
  display_name = "zat Cloud Run Job runtime"
}

resource "google_service_account" "scheduler" {
  account_id   = "zat-scheduler"
  display_name = "zat Cloud Scheduler invoker"
}

resource "google_service_account" "deployer" {
  account_id   = "zat-deployer"
  display_name = "zat GitHub Actions deployer (WIF)"
}
```

- [ ] **Step 2: Verify**

Run:
```bash
cd /Users/pmoust/nofire/engineering/zat/terraform
terraform fmt -check && terraform validate
```
Expected: `Success! The configuration is valid.`

- [ ] **Step 3: Commit**

```bash
cd /Users/pmoust/nofire/engineering
git add zat/terraform/service_accounts.tf
git commit -m "zat: terraform service accounts (runtime, scheduler, deployer)"
```

---

### Task 8: Terraform — IAM (WIF, project roles, secret accessors)

**Files:**
- Create: `/Users/pmoust/nofire/engineering/zat/terraform/iam.tf`

- [ ] **Step 1: Write `iam.tf`**

```hcl
# --- Workload Identity Federation for GitHub Actions -------------------------
resource "google_iam_workload_identity_pool" "github" {
  workload_identity_pool_id = "github-pool"
  display_name              = "GitHub Actions pool"
}

resource "google_iam_workload_identity_pool_provider" "github" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.github.workload_identity_pool_id
  workload_identity_pool_provider_id = "github-provider"
  display_name                       = "GitHub OIDC"

  attribute_mapping = {
    "google.subject"       = "assertion.sub"
    "attribute.repository" = "assertion.repository"
  }

  # Only tokens from the engineering repo may map into this provider.
  attribute_condition = "assertion.repository == \"${var.github_repo}\""

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }
}

# Allow the engineering repo to impersonate the deployer SA.
resource "google_service_account_iam_member" "deployer_wif" {
  service_account_id = google_service_account.deployer.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github.name}/attribute.repository/${var.github_repo}"
}

# --- Deployer SA project roles (terraform apply + image push + secret add) ---
locals {
  deployer_roles = [
    "roles/run.admin",
    "roles/artifactregistry.admin",
    "roles/secretmanager.admin",
    "roles/cloudscheduler.admin",
    "roles/iam.serviceAccountAdmin",
    "roles/iam.serviceAccountUser",
    "roles/iam.workloadIdentityPoolAdmin",
    "roles/resourcemanager.projectIamAdmin",
    "roles/serviceusage.serviceUsageAdmin",
  ]
}

resource "google_project_iam_member" "deployer" {
  for_each = toset(local.deployer_roles)
  project  = var.project_id
  role     = each.value
  member   = "serviceAccount:${google_service_account.deployer.email}"
}

# --- Runtime SA: read the four secrets ---------------------------------------
resource "google_secret_manager_secret_iam_member" "runtime_access" {
  for_each  = google_secret_manager_secret.zat
  secret_id = each.value.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.runtime.email}"
}

# --- Scheduler SA: run the job -----------------------------------------------
resource "google_cloud_run_v2_job_iam_member" "scheduler_invoker" {
  name     = google_cloud_run_v2_job.zat.name
  location = var.region
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.scheduler.email}"
}
```

- [ ] **Step 2: Verify**

Run:
```bash
cd /Users/pmoust/nofire/engineering/zat/terraform
terraform fmt -check && terraform validate
```
Expected: `Success! The configuration is valid.` (This references `google_cloud_run_v2_job.zat`, defined next in Task 9; if you run validate now it will error on the unknown resource. Either add Task 9 before validating, or temporarily comment the `scheduler_invoker` block. The committed state after Task 9 validates cleanly.)

- [ ] **Step 3: Commit**

```bash
cd /Users/pmoust/nofire/engineering
git add zat/terraform/iam.tf
git commit -m "zat: terraform IAM (WIF, deployer roles, secret accessors, job invoker)"
```

---

### Task 9: Terraform — Cloud Run Job

**Files:**
- Create: `/Users/pmoust/nofire/engineering/zat/terraform/cloud_run_job.tf`

- [ ] **Step 1: Write `cloud_run_job.tf`** (each secret is its own volume; entrypoint copies them to `/tmp/config`)

```hcl
resource "google_cloud_run_v2_job" "zat" {
  name     = "zat"
  location = var.region

  template {
    template {
      service_account = google_service_account.runtime.email
      max_retries     = 1
      timeout         = "900s"

      containers {
        image = local.image
        args = [
          "-no-server",
          "-t", "recording,transcript",
          "-since", "24h",
          "-min-duration", "5",
        ]

        resources {
          limits = {
            cpu    = "1"
            memory = "512Mi"
          }
        }

        volume_mounts {
          name       = "zat-config"
          mount_path = "/secrets/zat-config"
        }
        volume_mounts {
          name       = "google-config"
          mount_path = "/secrets/google-config"
        }
        volume_mounts {
          name       = "google-creds"
          mount_path = "/secrets/google-creds"
        }
        volume_mounts {
          name       = "slack-config"
          mount_path = "/secrets/slack-config"
        }
      }

      volumes {
        name = "zat-config"
        secret {
          secret = google_secret_manager_secret.zat["config"].secret_id
          items {
            version = "latest"
            path    = "zat.yml"
          }
        }
      }
      volumes {
        name = "google-config"
        secret {
          secret = google_secret_manager_secret.zat["google_config"].secret_id
          items {
            version = "latest"
            path    = "google.config.json"
          }
        }
      }
      volumes {
        name = "google-creds"
        secret {
          secret = google_secret_manager_secret.zat["google_creds"].secret_id
          items {
            version = "latest"
            path    = "google.creds.json"
          }
        }
      }
      volumes {
        name = "slack-config"
        secret {
          secret = google_secret_manager_secret.zat["slack_config"].secret_id
          items {
            version = "latest"
            path    = "slack.config.json"
          }
        }
      }
    }
  }

  # The image tag changes outside terraform on most deploys; ignore drift so a
  # plain `terraform apply` doesn't fight the deploy workflow's image bumps.
  lifecycle {
    ignore_changes = [
      template[0].template[0].containers[0].image,
    ]
  }

  depends_on = [google_secret_manager_secret_iam_member.runtime_access]
}
```

- [ ] **Step 2: Verify** (now that the job exists, the Task 8 `scheduler_invoker` reference resolves)

Run:
```bash
cd /Users/pmoust/nofire/engineering/zat/terraform
terraform fmt -check && terraform validate
```
Expected: `Success! The configuration is valid.`

- [ ] **Step 3: Commit**

```bash
cd /Users/pmoust/nofire/engineering
git add zat/terraform/cloud_run_job.tf
git commit -m "zat: terraform Cloud Run Job with secret-volume config"
```

---

### Task 10: Terraform — Scheduler and outputs

**Files:**
- Create: `/Users/pmoust/nofire/engineering/zat/terraform/scheduler.tf`
- Create: `/Users/pmoust/nofire/engineering/zat/terraform/outputs.tf`

- [ ] **Step 1: Write `scheduler.tf`** (OAuth token because the target is a Google API; the v2 admin `:run` endpoint)

```hcl
resource "google_cloud_scheduler_job" "zat" {
  name      = "zat-archival"
  region    = var.region
  schedule  = var.schedule
  time_zone = var.scheduler_time_zone

  attempt_deadline = "320s"

  http_target {
    http_method = "POST"
    uri         = "https://${var.region}-run.googleapis.com/v2/projects/${var.project_id}/locations/${var.region}/jobs/${google_cloud_run_v2_job.zat.name}:run"

    oauth_token {
      service_account_email = google_service_account.scheduler.email
    }
  }
}
```

- [ ] **Step 2: Write `outputs.tf`** (values used by bootstrap docs and GitHub repo secrets)

```hcl
output "image" {
  description = "Fully-qualified Artifact Registry image reference for the deployed tag."
  value       = local.image
}

output "workload_identity_provider" {
  description = "Set as GitHub secret GCP_WIF_PROVIDER for the deploy/config workflows."
  value       = google_iam_workload_identity_pool_provider.github.name
}

output "deployer_service_account" {
  description = "Set as GitHub secret GCP_DEPLOYER_SA."
  value       = google_service_account.deployer.email
}

output "job_name" {
  value = google_cloud_run_v2_job.zat.name
}

output "config_secret_id" {
  description = "Secret that the config workflow adds zat.yml versions to."
  value       = google_secret_manager_secret.zat["config"].secret_id
}
```

- [ ] **Step 3: Verify**

Run:
```bash
cd /Users/pmoust/nofire/engineering/zat/terraform
terraform fmt -check && terraform validate
```
Expected: `Success! The configuration is valid.`

- [ ] **Step 4: Commit**

```bash
cd /Users/pmoust/nofire/engineering
git add zat/terraform/scheduler.tf zat/terraform/outputs.tf
git commit -m "zat: terraform Cloud Scheduler trigger + outputs"
```

---

### Task 11: GitHub Actions — config fast path

**Files:**
- Create: `/Users/pmoust/nofire/engineering/.github/workflows/zat-config.yml`

- [ ] **Step 1: Create the workflows directory**

```bash
mkdir -p /Users/pmoust/nofire/engineering/.github/workflows
```

- [ ] **Step 2: Write `zat-config.yml`** (on `zat/zat.yml` change: add a secret version, then prune to ≤6 enabled versions to stay in the free tier)

```yaml
name: zat-config

on:
  push:
    branches: [main]
    paths:
      - zat/zat.yml

permissions:
  contents: read
  id-token: write

jobs:
  publish-config:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - id: auth
        uses: google-github-actions/auth@v2
        with:
          workload_identity_provider: ${{ secrets.GCP_WIF_PROVIDER }}
          service_account: ${{ secrets.GCP_DEPLOYER_SA }}

      - uses: google-github-actions/setup-gcloud@v2

      - name: Add new zat-config version
        run: |
          gcloud secrets versions add zat-config \
            --project="${{ secrets.GCP_PROJECT_ID }}" \
            --data-file=zat/zat.yml

      - name: Prune old enabled versions (keep newest 6)
        run: |
          gcloud secrets versions list zat-config \
            --project="${{ secrets.GCP_PROJECT_ID }}" \
            --filter="state=enabled" \
            --sort-by=~createTime \
            --format="value(name)" | tail -n +7 | while read -r v; do
              echo "destroying version $v"
              gcloud secrets versions destroy "$v" \
                --project="${{ secrets.GCP_PROJECT_ID }}" \
                --secret=zat-config --quiet
            done
```

- [ ] **Step 3: Verify with actionlint**

Run: `actionlint /Users/pmoust/nofire/engineering/.github/workflows/zat-config.yml`
Expected: no output (exit 0).

- [ ] **Step 4: Commit**

```bash
cd /Users/pmoust/nofire/engineering
git add .github/workflows/zat-config.yml
git commit -m "zat: config workflow publishes zat.yml to Secret Manager"
```

---

### Task 12: GitHub Actions — infra/deploy path

**Files:**
- Create: `/Users/pmoust/nofire/engineering/.github/workflows/zat-deploy.yml`

- [ ] **Step 1: Write `zat-deploy.yml`** (on infra changes: build+push the pinned image, then plan on PRs / apply on main)

```yaml
name: zat-deploy

on:
  push:
    branches: [main]
    paths:
      - zat/Dockerfile
      - zat/entrypoint.sh
      - zat/ZAT_REF
      - zat/terraform/**
  pull_request:
    paths:
      - zat/Dockerfile
      - zat/entrypoint.sh
      - zat/ZAT_REF
      - zat/terraform/**

permissions:
  contents: read
  id-token: write

env:
  REGION: us-central1
  PROJECT_ID: nofire-production

jobs:
  deploy:
    runs-on: ubuntu-latest
    defaults:
      run:
        working-directory: zat
    steps:
      - uses: actions/checkout@v4

      - name: Read pinned ref
        id: ref
        run: echo "zat_ref=$(tr -d '[:space:]' < ZAT_REF)" >> "$GITHUB_OUTPUT"

      - id: auth
        uses: google-github-actions/auth@v2
        with:
          workload_identity_provider: ${{ secrets.GCP_WIF_PROVIDER }}
          service_account: ${{ secrets.GCP_DEPLOYER_SA }}

      - uses: google-github-actions/setup-gcloud@v2

      - name: Configure docker auth
        run: gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet

      - name: Build and push image
        run: |
          IMAGE="${REGION}-docker.pkg.dev/${PROJECT_ID}/zat/zat:${{ steps.ref.outputs.zat_ref }}"
          docker build \
            --build-arg ZAT_REF="${{ steps.ref.outputs.zat_ref }}" \
            -t "$IMAGE" .
          docker push "$IMAGE"

      - uses: hashicorp/setup-terraform@v3

      - name: Terraform init
        working-directory: zat/terraform
        run: terraform init -backend-config="bucket=${{ secrets.GCP_TF_STATE_BUCKET }}"

      - name: Terraform plan
        working-directory: zat/terraform
        run: terraform plan -var="image_tag=${{ steps.ref.outputs.zat_ref }}" -input=false

      - name: Terraform apply
        if: github.ref == 'refs/heads/main' && github.event_name == 'push'
        working-directory: zat/terraform
        run: terraform apply -auto-approve -var="image_tag=${{ steps.ref.outputs.zat_ref }}" -input=false

      - name: Deploy new image to job
        if: github.ref == 'refs/heads/main' && github.event_name == 'push'
        run: |
          gcloud run jobs update zat \
            --project="${PROJECT_ID}" \
            --region="${REGION}" \
            --image="${REGION}-docker.pkg.dev/${PROJECT_ID}/zat/zat:${{ steps.ref.outputs.zat_ref }}"
```

- [ ] **Step 2: Verify with actionlint**

Run: `actionlint /Users/pmoust/nofire/engineering/.github/workflows/zat-deploy.yml`
Expected: no output (exit 0).

- [ ] **Step 3: Commit**

```bash
cd /Users/pmoust/nofire/engineering
git add .github/workflows/zat-deploy.yml
git commit -m "zat: deploy workflow builds image and applies terraform"
```

---

### Task 13: README / bootstrap documentation

**Files:**
- Create: `/Users/pmoust/nofire/engineering/zat/README.md`

- [ ] **Step 1: Write `zat/README.md`**

````markdown
# zat — Meet archival on Cloud Run

`zat` copies Google Meet recordings & transcripts into mapped Google Drive folders
and posts a Slack notification, on a 2-hourly schedule, at ~$0 cost. It runs as a
Cloud Run Job in `nofire-production`, configured entirely from this directory.

## Editing what gets archived

Edit [`zat.yml`](./zat.yml) and open a PR. Each entry maps a meeting:

```yaml
- name: my-weekly          # label used in archived folder/file names
  google: <drive-folder-id> # destination Drive folder ID
  meet: abc-defg-hij        # the code from meet.google.com/abc-defg-hij
  slack: C01234567          # Slack channel ID to notify
```

On merge to `main`, the `zat-config` workflow publishes the file as a new Secret
Manager version. The change takes effect on the **next 2-hourly run** — no deploy.

> The archival account only sees Meet conferences it participates in. If you add a
> meeting hosted by someone that account cannot see, archival for it silently does
> nothing.

## How it works

- **Cloud Run Job `zat`** runs `zat -no-server -t recording,transcript -since 24h`
  every 2 hours (Cloud Scheduler).
- Config + credentials live in **Secret Manager** (`zat-config`, `zat-google-config`,
  `zat-google-creds`, `zat-slack-config`), mounted read-only and copied to a writable
  dir by `entrypoint.sh`.
- The image is built from [`pmoust/zat`](https://github.com/pmoust/zat) at the git
  ref in [`ZAT_REF`](./ZAT_REF). Bump that file to upgrade zat.
- GitHub Actions authenticate to GCP via keyless Workload Identity Federation.

## Cost

Cloud Run Jobs (a few ~1-min runs/day), Cloud Scheduler (1 of 3 free jobs), Secret
Manager (≤6 versions), Artifact Registry (~8 MB image, cleanup keeps 3), GCS state
(KBs), and Logging all sit within Google's always-free tiers. Steady-state cost
rounds to zero.

## One-time bootstrap (admin)

Run by a person with admin on `nofire-production`. CI cannot do this first apply
because it creates the very identity CI uses.

1. **Push the zat source** and record the ref:
   ```bash
   cd /path/to/pmoust/zat && git push origin main && git rev-parse HEAD
   ```
   Put that SHA in [`ZAT_REF`](./ZAT_REF) and as `image_tag` in your tfvars.

2. **Enable APIs:**
   ```bash
   gcloud services enable run.googleapis.com cloudscheduler.googleapis.com \
     secretmanager.googleapis.com artifactregistry.googleapis.com \
     iam.googleapis.com iamcredentials.googleapis.com \
     drive.googleapis.com meet.googleapis.com --project nofire-production
   ```

3. **Create the Terraform state bucket** (once):
   ```bash
   gcloud storage buckets create gs://nofire-zat-tfstate \
     --project nofire-production --location us-central1 --uniform-bucket-level-access
   gcloud storage buckets update gs://nofire-zat-tfstate --versioning
   ```

4. **First apply** (also builds/pushes the first image — see step 6 ordering):
   ```bash
   cd zat/terraform
   terraform init -backend-config="bucket=nofire-zat-tfstate"
   terraform apply -var="image_tag=<SHA>"
   ```
   The Cloud Run Job references the image; build & push it (step 6) before the job
   can execute successfully.

5. **Load the credential secrets** from your working local zat config:
   ```bash
   gcloud secrets versions add zat-google-config --data-file=google.config.json
   gcloud secrets versions add zat-google-creds  --data-file=google.creds.json
   gcloud secrets versions add zat-slack-config  --data-file=slack.config.json
   gcloud secrets versions add zat-config         --data-file=zat/zat.yml
   ```

6. **Build & push the first image** (CI does this later automatically):
   ```bash
   cd zat
   gcloud auth configure-docker us-central1-docker.pkg.dev
   docker build --build-arg ZAT_REF=<SHA> -t us-central1-docker.pkg.dev/nofire-production/zat/zat:<SHA> .
   docker push us-central1-docker.pkg.dev/nofire-production/zat/zat:<SHA>
   ```

7. **Set GitHub repo secrets** in `nofireai/engineering` (from `terraform output`):
   `GCP_WIF_PROVIDER`, `GCP_DEPLOYER_SA`, `GCP_PROJECT_ID=nofire-production`,
   `GCP_TF_STATE_BUCKET=nofire-zat-tfstate`.

8. **Smoke test:**
   ```bash
   gcloud run jobs execute zat --region us-central1 --project nofire-production --wait
   ```
   Confirm artifacts appear in Drive and a Slack message posts. Inspect logs:
   ```bash
   gcloud run jobs executions list --job zat --region us-central1
   ```

## Upgrading zat

Bump [`ZAT_REF`](./ZAT_REF) to a new `pmoust/zat` SHA and merge — `zat-deploy`
rebuilds, pushes, and rolls the job to the new image.
````

- [ ] **Step 2: Verify markdown renders / links exist**

Run: `ls /Users/pmoust/nofire/engineering/zat/{zat.yml,ZAT_REF,entrypoint.sh}`
Expected: all three paths listed (the README links resolve).

- [ ] **Step 3: Commit**

```bash
cd /Users/pmoust/nofire/engineering
git add zat/README.md
git commit -m "zat: bootstrap + usage documentation"
```

---

### Task 14: Final whole-config verification

- [ ] **Step 1: Re-run all static checks together**

```bash
cd /Users/pmoust/nofire/engineering/zat/terraform
terraform fmt -check && terraform init -backend=false && terraform validate
shellcheck /Users/pmoust/nofire/engineering/zat/entrypoint.sh
actionlint /Users/pmoust/nofire/engineering/.github/workflows/zat-config.yml \
           /Users/pmoust/nofire/engineering/.github/workflows/zat-deploy.yml
```
Expected: terraform `Success!`, shellcheck and actionlint silent.

- [ ] **Step 2: Confirm clean git state**

Run: `cd /Users/pmoust/nofire/engineering && git status`
Expected: working tree clean, all zat files committed.

- [ ] **Step 3 (manual, post-merge): execute the live bootstrap** following `zat/README.md`, then trigger a real run and confirm a Drive artifact + Slack message. This is the only end-to-end test and requires the live project.

---

## Self-review notes

- **Spec coverage:** Cloud Run Job + Scheduler (Tasks 9–10); zat.yml in engineering repo (Task 1); Secret Manager mounted config (Tasks 6, 9); auto-update on zat.yml edit (Task 11, mount `latest`); image from pinned `pmoust/zat` (Tasks 3, 12); WIF keyless auth (Task 8, 11, 12); terraform + docs co-located (Tasks 4–10, 13); ~$0 cost (cleanup policy Task 5, version prune Task 11, free-tier resources). All covered.
- **Read-only mount problem:** handled by `entrypoint.sh` (Task 2) — the design's key technical risk.
- **Bootstrap chicken-and-egg:** first apply is manual (Task 13); CI handles steady state.
- **Known runtime assumption:** single-account Meet visibility — documented in README, not a code change here.
