# zat on Cloud Run — config-driven, auto-deployed, ~$0 forever

**Date:** 2026-06-10
**Status:** Approved design (pre-implementation)

## Goal

Run `zat` (Google Meet recording + transcript archival to Drive, with Slack
notifications) as an unattended, scheduled job on Google Cloud at near-zero cost,
driven by a `zat.yml` config that engineers edit in the `nofireai/engineering`
repo. Editing `zat.yml` propagates the new configuration automatically. Image
builds and infrastructure changes deploy automatically via GitHub Actions in
`nofireai/engineering`.

## Constraints & decisions

| Decision | Choice | Rationale |
|---|---|---|
| Runtime model | **Cloud Run Job + Cloud Scheduler** | `zat` is a batch task (`-no-server`: archive then exit), not a request server. Job + Scheduler is `$0` at idle. |
| Config delivery | **`zat.yml` as a Secret Manager version, mounted as a file** | Single mechanism for config + secrets; no code change to zat; mount `latest` so each run picks up newest config. |
| Archival scope | **Meet-only** | Matches current `zat.yml`; fewer secrets; no Zoom OAuth. Zoom can be added later. |
| Google identity | **Reuse existing local refresh token** | The account already running zat locally sees these meetings; lowest risk. zat Meet support only sees conferences for the single signed-in account. |
| Image source | **`nofireai/engineering` builds from pinned `pmoust/zat` ref** | Deployment lives in one repo; bump `ZAT_REF` to upgrade; no changes to the zat repo. |
| Target project | **`nofire-production`** | Same project as the existing OAuth client. |
| Cadence | **Every 2 hours, `-since 24h`** | 12× overlap absorbs Meet artifact-generation delay; Drive-listing dedup makes re-processing idempotent and free. |

### Explicit YAGNI cuts

- No Zoom support.
- No domain-wide delegation / org-wide multi-host archival (zat does not implement it).
- No GCS config bucket (Secret Manager covers config).
- No persistent state DB (dedup is done by listing the target Drive folder each run).
- No always-on Cloud Run Service.

## Architecture

```
Engineer edits zat.yml ─merge→ GitHub Action (zat-config) ─→ new Secret Manager version (zat-config)
                                                                      │  (mounted "latest")
Cloud Scheduler ──cron (every 2h)──→ Cloud Run Job "zat" ──reads──→ Secret Manager
                                          │  entrypoint copies /secrets/* → /tmp/config (writable)
                                          │  zat -no-server -config-dir /tmp/config -t recording,transcript -since 24h
                                          ▼
                                 Google Meet API + Drive API,  Slack API
```

### Why this is ~$0 forever

- **Cloud Run Jobs:** a handful of ~1-minute runs/day, far under the free tier
  (180k vCPU-seconds, 360k GiB-seconds/month).
- **Cloud Scheduler:** uses 1 of 3 free jobs.
- **Secret Manager:** ≤6 active versions kept and low access volume — within free tier.
- **Artifact Registry:** distroless Go image (~30 MB) with a cleanup policy keeping
  the last few images — under the 0.5 GB free allowance.
- **GCS (Terraform state):** kilobytes.
- **Cloud Logging:** under the 50 GB/month free tier.

Nothing runs at idle, so steady-state cost rounds to zero.

## Repository layout (`nofireai/engineering`)

```
zat/
  zat.yml                    # the file engineers edit
  Dockerfile                 # multi-stage build of pmoust/zat @ ZAT_REF, distroless base + entrypoint
  entrypoint.sh              # copies /secrets/* → /tmp/config, then exec zat
  terraform/
    main.tf                  # providers, GCS backend
    variables.tf
    outputs.tf
    artifact_registry.tf     # Docker repo + cleanup policy (keep last N)
    secrets.tf               # 4 secrets (config + 3 credential secrets)
    cloud_run_job.tf         # job definition, secret mounts, runtime SA, args
    scheduler.tf             # cron → Cloud Run jobs.run with OIDC
    iam.tf                   # runtime SA, scheduler SA, GitHub Workload Identity Federation
  README.md                  # how to edit zat.yml, one-time bootstrap, cost notes
.github/workflows/           # (repo root — GitHub only reads workflows from here)
  zat-config.yml             # paths: zat/zat.yml → add secret version, prune old
  zat-deploy.yml             # paths: zat/Dockerfile|entrypoint.sh|terraform/** → build image + terraform apply
```

## Components

### Cloud Run Job `zat`

- Image: from Artifact Registry, built from `pmoust/zat@ZAT_REF`.
- Command/args: `zat -no-server -config-dir /tmp/config -t recording,transcript -since 24h -min-duration 5`.
- Resources: 512 MiB / 1 vCPU, small retry count, generous task timeout.
- Runs as a dedicated **runtime service account** with only `secretmanager.secretAccessor`
  on the four zat secrets — least privilege.
- Mounts all four secrets read-only under `/secrets/`.

### Entrypoint wrapper (`entrypoint.sh`)

Secret Manager mounts are **read-only**, but `zat` writes `google.creds.json` back
on every token refresh (`HasCreds()` → `updateCreds` → `saveCreds`; access tokens
expire hourly so this fires nearly every run). The wrapper copies the four mounted
secrets from `/secrets/` into writable `/tmp/config`, then `exec`s
`zat -config-dir /tmp/config ...`. Refreshed access tokens are written to ephemeral
`/tmp` and discarded on exit — harmless, because the long-lived refresh token persists
in the `zat-google-creds` secret. No change to zat's Go code.

**Known edge case:** if Google ever rotates the refresh token itself (uncommon for
this OAuth client type), the rotated value would be lost on exit and the next run
could need a re-bootstrap. Accepted as low-risk; persisting back to the secret is a
possible future enhancement (adds IAM + complexity).

### Secrets in Secret Manager

| Secret | Contents | Set by |
|---|---|---|
| `zat-config` | `zat.yml` body | `zat-config.yml` workflow on every edit |
| `zat-google-config` | OAuth client id/secret (`google.config.json`) | one-time bootstrap |
| `zat-google-creds` | refresh token (`google.creds.json`) | one-time bootstrap (existing local token) |
| `zat-slack-config` | Slack bot token (`slack.config.json`) | one-time bootstrap |

`zat-config` is mounted at `latest`; new versions apply on the next scheduled run
with no redeploy. The workflow prunes old `zat-config` versions to keep ≤6 active.

### Cloud Scheduler

A single cron job (`0 */2 * * *`) calls the Cloud Run Admin API
(`jobs/zat:run`) authenticated with OIDC via a dedicated scheduler service account
that holds `run.invoker` / `run.developer` scoped to the job.

### CI/CD & authentication

- **GitHub → GCP: Workload Identity Federation (keyless OIDC).** A dedicated
  deployer service account is impersonated only from the `nofireai/engineering`
  repository — no long-lived SA JSON keys stored in GitHub.
- **`zat-config.yml`** (fast path): triggers only on `zat/zat.yml` changes →
  `gcloud secrets versions add zat-config --data-file=...` then prune to ≤6 active
  versions. No image build. New config applies on the next scheduled run.
- **`zat-deploy.yml`** (infra path): triggers on `Dockerfile`, `entrypoint.sh`,
  `terraform/**`, or a `ZAT_REF` bump → build image from `pmoust/zat@ZAT_REF`, push
  to Artifact Registry, then `terraform apply`.

## Data flow

1. Scheduler fires every 2h → triggers the Job.
2. Job container starts; entrypoint copies the four mounted secrets to `/tmp/config`.
3. `zat -no-server` loads `zat.yml`, refreshes the Google access token from the
   stored refresh token, lists recent Meet conferences (last 24h) for the signed-in
   account.
4. For each mapped meeting, it lists the target Drive folder to dedup, copies any
   new recording/transcript artifacts, and posts a Slack notification to the mapped
   channel.
5. Container exits. Logs land in Cloud Logging.

## Error handling

- Failures surface in Cloud Logging and the job execution status.
- The job is idempotent (Drive-listing dedup), so a failed run self-heals on the
  next cron tick.
- Optional: a log-based alert (free) on run errors.

## Testing

- CI runs `terraform validate` and `terraform plan` on infra changes.
- Bootstrap includes a manual smoke test: `gcloud run jobs execute zat` and confirm
  artifacts land in Drive + a Slack message posts.
- `entrypoint.sh` kept trivial and reviewable.

## One-time bootstrap (documented in `zat/README.md`)

1. Confirm GCP project `nofire-production`; enable APIs: Cloud Run, Cloud Scheduler,
   Secret Manager, Artifact Registry, Drive, Meet, IAM.
2. Configure the Terraform GCS backend bucket.
3. `terraform apply` once — creates WIF, service accounts, Artifact Registry,
   secret shells, the Job, and the Scheduler.
4. Load the three credential secrets from the existing working `google.config.json`,
   `google.creds.json`, `slack.config.json`.
5. Push `zat.yml` → first `zat-config` version.
6. Build/push the first image (run `zat-deploy.yml` or build manually).
7. Smoke test: `gcloud run jobs execute zat`.

## Open items / future enhancements

- Persist a rotated refresh token back to Secret Manager (only if rotation proves to
  be an issue).
- Add Zoom archival (extra secrets + Zoom OAuth bootstrap).
- Org-wide multi-host archival once zat implements domain-wide delegation.
