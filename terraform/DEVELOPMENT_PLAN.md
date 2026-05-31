# SeaweedFS Terraform Support — Development Plan

Status: proposal. Audience: SeaweedFS maintainers and the Terraform-module authors. Scope: first-class, self-contained Terraform support for deploying SeaweedFS on cloud VMs/instances running the `weed` binary directly under systemd, with no Helm and no Kubernetes dependency.

A standing principle runs through this plan: the Helm chart is the **structural** reference (knob names, component relationships, ports, file layout) — it is **not** the reference for security defaults or flag defaults. We re-derive every flag default from the `weed` binary (`weed/command/*.go`), not from `values.yaml`, and we **harden** every secret default rather than mirror the chart's placeholders. Both points are load-bearing and surface repeatedly below.

---

## 1. Goals, Non-Goals, Audience, Success Criteria

### Goals
- Provision the full underlying infrastructure (VMs, block disks, networking, load balancers, firewalls, DNS) and run `weed` daemons directly via cloud-init + systemd. No Kubernetes, no Helm, no container runtime required.
- Cover every production topology the Helm chart covers: standalone all-in-one, and distributed master/volume/filer plus optional s3, sftp, admin, worker — including multi-DC/zone-aware **named volume groups**.
- Single-source the SeaweedFS deployment knowledge (flags, ports, file layout, ordering) in a cloud-agnostic core so per-cloud wrappers stay thin.
- Publish to the Terraform Registry (and by extension the OpenTofu Registry) as `terraform-aws-seaweedfs`, `terraform-google-seaweedfs`, `terraform-azurerm-seaweedfs`, with a versioned compatibility matrix against `weed` releases.
- Keep the configuration **shape** faithful to the chart's `values.yaml` so a chart user can find the equivalent knob — while hardening security defaults and re-deriving flag defaults from the binary.

### Non-Goals
- Not wrapping Helm or templating Kubernetes manifests from Terraform. The chart is the reference for *what* to configure, not an artifact we invoke.
- COSI's Kubernetes plumbing (BucketClaim CRD lifecycle, the K8s API server dependency) is out of scope for the VM track. **But the SeaweedFS data-management capabilities COSI exposed are not dropped** — see §3 for routing tiered-storage/disk-type bucket parameters into the bucket-bootstrap path / data-plane provider.
- No cross-region object replication orchestration in v1 (document the multi-cluster `WEED_CLUSTER_*` pattern; do not automate it).
- No automatic, blind autoscaling of stateful tiers (masters, volumes). Only stateless gateways (s3, worker) get ASG/MIG autoscaling.
- The Rust volume server (`/usr/bin/weed-volume`) is supported as an opt-in flag but is treated as experimental, matching `volume.rust` default `false`.
- **No committed secret material, anywhere.** No private keys, passwords, or signing keys as defaults in module code or examples. This is a hard guardrail, not a recommendation (see §6).

### Target audience
Companies whose primary IaC is Terraform/OpenTofu and who do not run (or do not want to depend on) Kubernetes for storage. They expect: object-typed variables, `sensitive` handling, registry-published modules, `terraform plan`-level safety, and the ability to bolt SeaweedFS into an existing VPC/estate.

### Success criteria
- `terraform apply` of the AWS HA example yields a 3-master Raft quorum (verified: exactly one master reports `IsLeader:true` via `/cluster/status` and the rest agree on the same `Leader`; `/cluster/healthz` returns 200 on a quorum), volume servers registered, a filer serving `GET /`, and an S3 gateway answering `GET /status` and an **authenticated** PutObject/GetObject round-trip (auth enabled) — with zero manual steps.
- The flagship HA example is **secure-by-default**: `enable_security=true` (mTLS), S3 `enable_auth=true`, admin password required, JWT signing on, metrics bound to private IPs.
- Destroying and re-creating a single volume instance re-attaches the same disks **at the same stable address** and the master re-registers it without data loss. (Conditional for EC volumes — see §5 EC durability contract.)
- A master binary upgrade can be performed one node at a time without losing quorum, per a documented runbook that uses the real raft membership API (`cluster.raft.add`/`cluster.raft.remove`/`cluster.raft.ps`), not a `replicas` bump.
- Modules pass `terraform fmt -check`, `validate`, `tflint`, a security scan (trivy/checkov), `terraform-docs` drift check, native `terraform test`, a committed-secret/weak-default scanner, and a per-cloud apply-level Terratest including a single-node-replace-preserves-data assertion.
- Same git tags consume cleanly on `registry.terraform.io` and `registry.opentofu.org`, validated under both the declared `required_version` floor and latest, for both `terraform` and `tofu`.

---

## 2. Architecture & Module Decomposition

### Layered design — a one-way pipeline, not a cycle
Two hard layers, mirroring the proven HashiCorp (`install-consul`/`run-consul`/`consul-cluster`) and ScyllaDB/MinIO-style split. **Critical correction over the naive layering:** addressing is an **input** to the core, computed by the wrapper first. The core creates **zero** cloud resources and is a pure function of `(addresses + config)`. This breaks the apply-time dependency cycle that an "address-computing core" would create.

```
per-cloud wrapper:  reserve static IPs / DNS  ─┐
                                               ▼
core:  (address_map + config) ─► render certs (SANs from address_map) ─► render cloud-init/systemd/config  ─► outputs
                                               │
per-cloud wrapper:  create instances with that user_data ◄────────────┘ (one-way; no back-edge)
```

1. **Cloud-agnostic core (`install/run` + `tls`/`secrets`)** — owns everything portable: rendering each role's `weed` flags into a systemd `ExecStart`, the cloud-init parts, config files (`master.toml`, `filer.toml`, `security.toml`, `notification.toml`, S3 identity JSON, SFTP user JSON), and PKI/secret generation **keyed by the passed-in address map**. Pure functions of inputs; emits rendered artifacts as outputs. Implemented as `null`/`cloudinit`/`tls`/`random`-only Terraform plus `templatefile()` templates. Creates no cloud resources.

2. **Per-cloud infra wrappers** — `terraform-aws-seaweedfs`, `terraform-google-seaweedfs`, `terraform-azurerm-seaweedfs`. Each (a) reserves stable addressing **first**, (b) calls the core with that address map to render config/certs, (c) creates instances/disks/VPC/SGs/LBs/DNS/IAM/secret-store entries, injecting the rendered config. The cross-module dependency is strictly one-way.

VM-first is justified because the audience explicitly refuses a K8s dependency, clients fetch data **directly from volume servers** (not through a gateway), and SeaweedFS's stateful tiers (Raft masters, disk-owning volume servers) are pets, not cattle — they map poorly onto free-scaling abstractions. This is the same conclusion ScyllaDB's module reached.

### Stateful tiers use keyed `for_each`, never `count`
This is an **API-shape decision made up front**, not a documentation note. `replicas:number` forces `count`, and removing/replacing master index 1 of `[0,1,2]` reindexes `2→1` — destroying the wrong instance, re-templating the wrong `-peers` entry, re-attaching the wrong disk. The fix is structural:

| Tier | API | Terraform construct |
|---|---|---|
| master | `var.master.nodes = map(object({ static_ip?, subnet/zone, ... }))` keyed `m0/m1/m2` | `for_each` over stable keys |
| volume | `var.volume.node_groups` / `var.volume_groups = map(object({...}))` keyed `vol-a/vol-b` | `for_each` over stable keys |
| filer | `var.filer.nodes = map(object({...}))` keyed | `for_each` |
| s3, worker (stateless) | `replicas:number` or ASG/MIG | `count` / autoscaler |

"Odd quorum" validation becomes a check on `length(keys(var.master.nodes))`. A `tftest` asserts that removing a **middle** key plans `destroy` of **only** that node. This belongs in M0/M1, not M4.

### Repo strategy & core distribution
The Registry naming key `terraform-<PROVIDER>-<NAME>` ties one repo to one provider, so there is one published repo per cloud. The canonical core lives **in this monorepo** under `terraform/modules/core/` so it stays in lockstep with `weed` flag changes. For Registry consumption we must avoid a frozen-vendored-core-that-drifts trap:

- **Preferred:** publish the core as its own registry module (`terraform-seaweedfs-core`, a null-provider module). Each wrapper depends on it via a **version-pinned** `source = "...core"` constraint, so the `(wrapper, core, weed)` triple is explicit and reproducible.
- **If vendoring (git-subtree) is kept** for lockstep: make the mirror a **CI-enforced** step on every wrapper release that fails if the vendored core != the monorepo core at the wrapper's pinned core SHA.

Either way, each wrapper README carries a compatibility table: **wrapper tag → core tag → tested `weed` versions**.

### Proposed tree (in this repo, alongside `k8s/`)

```
terraform/
  README.md                       # overview, topology matrix, weed-version + core compat tables
  modules/
    core/                         # cloud-agnostic install/run/config renderer (zero cloud resources)
      main.tf                     # computes peers list, addresses, flag strings FROM address_map input
      variables.tf                # var.master/volume/volume_groups/filer/s3/sftp/admin/worker/all_in_one/global/security/hardening + address_map
      outputs.tf                  # rendered cloud-init per role, systemd unit text, config file text (sensitive where secret-bearing)
      versions.tf
      templates/
        cloud-init.yaml.tftpl     # write_files (non-secret only) + runcmd, multi-part
        weed-master.service.tftpl
        weed-volume.service.tftpl
        weed-filer.service.tftpl
        weed-s3.service.tftpl
        weed-sftp.service.tftpl
        weed-admin.service.tftpl
        weed-worker.service.tftpl
        weed-server.service.tftpl # all-in-one
        master.toml.tftpl
        filer.toml.tftpl
        security.toml.tftpl
        notification.toml.tftpl
        s3_config.json.tftpl
        sftp_config.json.tftpl
        weed.env.tftpl            # EnvironmentFile (WEED_* + secrets fetched at boot)
        fetch-secrets.sh.tftpl    # boot-time secret-store fetch + render (per cloud injected)
        mount-disks.sh.tftpl      # blkid-guarded mkfs + fstab + idx move
      tls/                        # CA + per-component certs with DISTINCT CNs (cert-manager replacement)
      secrets/                    # JWT/S3/admin/DB/SFTP credential generation (random_*), get-or-keep semantics
  modules-aws/                    # (lives in terraform-aws-seaweedfs repo; mirrored here for dev)
    main.tf instances.tf disks.tf network.tf security_groups.tf loadbalancer.tf dns.tf iam.tf secrets.tf outputs.tf variables.tf versions.tf
  examples/
    aws-all-in-one/
    aws-ha-distributed/           # secure-by-default
    aws-s3-only/
    gcp-ha-distributed/
    azure-ha-distributed/
  tests/
    core_unit.tftest.hcl          # plan-level, mock_provider
    flags_render.tftest.hcl       # ExecStart/-peers/security.toml/S3 JSON content
    index_shift.tftest.hcl        # removing a middle key destroys only that node
    terratest/                    # Go integration (real boot) — funded across M1/M2
  packer/                         # optional baked-image track (v2)
    seaweedfs.pkr.hcl
```

### Optional secondary tracks
- **Helm-free Kubernetes-native module** (`terraform-seaweedfs-kubernetes`): raw `kubernetes_manifest`/`kubernetes_stateful_set` resources using the `kubernetes` provider, for shops that run K8s **via Terraform** but reject Helm. Reuses the same config renderers. Lower priority; ship after the VM track is stable.
- **Native `terraform-provider-seaweedfs` (data plane)**: declarative buckets/identities/paths on top of a running cluster. Scoped and resourced as **its own roadmap with its own owner** (see §9), not bundled into a VM milestone. v0.1 ships **only** `seaweedfs_s3_bucket` + `seaweedfs_s3_identity`; `iam_policy`/`filer_path` deferred. Built on `terraform-plugin-framework`, hitting the S3 API + AWS-IAM-compatible endpoint on `:8333` and the filer HTTP API on `:8888`. Until then the `null_resource` bucket-bootstrap is the supported path. No VM-track milestone gates on it.

---

## 3. Configuration Surface Mapping

### Principle
Mirror the chart's `values.yaml` top-level sections onto **object-typed variables** using `optional(type, default)`, never a flat blob. The mirror is **structural only** — knob names and shape. **All flag defaults are re-derived from the binary** (`weed/command/*.go`), and **all security defaults are hardened**, not copied from the chart. Generate the `object()` defaults **mechanically from `values.yaml` for shape**, then overwrite each flag default with the binary's value; build the drift-diff CI check (§11) **first** and use it to author the schema, not just to police it.

Where we deliberately diverge from a chart default, it goes on an explicit **"deliberate deviation"** list (below), never a silent flip.

```hcl
variable "master" {
  type = object({
    nodes                 = map(object({                  # keyed for_each, NOT replicas:number
      static_ip = optional(string)                        # else wrapper-allocated
      zone      = optional(string)
      subnet_id = optional(string)
    }))
    port                  = optional(number, 9333)
    grpc_port             = optional(number, 19333)
    metrics_port          = optional(number, 9327)         # only emitted when monitoring enabled
    ip_bind               = optional(string, "0.0.0.0")
    mdir                  = optional(string, "/data")       # binary/chart uses /data; configurable, documented divergence if changed
    log_dir               = optional(string, "/var/log/seaweedfs")
    default_replication   = optional(string, "000")
    volume_size_limit_mb  = optional(number, 1000)
    volume_preallocate    = optional(bool, false)          # only emit flag when true
    garbage_threshold     = optional(number, null)          # null => omit flag => binary default
    disable_http          = optional(bool, false)          # only emit when true
    raft_hashicorp        = optional(bool, false)          # BINARY DEFAULT false (was wrong as true)
    election_timeout      = optional(string, "10s")
    heartbeat_interval    = optional(string, "300ms")
    resume_state          = optional(bool, true)           # deliberate deviation: binary default false; we default true for multi-master durability
    volume_growth         = optional(object({              # WEED_MASTER_VOLUME_GROWTH_COPY_*
      copy_1 = optional(number, 7), copy_2 = optional(number, 6),
      copy_3 = optional(number, 3), copy_other = optional(number, 1)
    }), {})
    extra_args            = optional(list(string), [])      # appended verbatim to ExecStart; bypasses plan-time checks
    extra_config_toml     = optional(string, "")            # raw master.toml block; bypasses plan-time checks
    log_level             = optional(number, null)
    secret_extra_environment_vars = optional(map(string), {}) # rendered into EnvironmentFile from secret store
  })
  default = {}
}
```

`bootstrap` is **not** a per-master field, and after the §4 raft correction it is **not** part of the steady-state path at all — it survives only as a documented, destructive break-glass recovery flag.

### Deliberate deviations from chart defaults (documented, not silent)
| Knob | Chart/binary default | Our default | Why |
|---|---|---|---|
| `master.resume_state` | `false` | `true` | multi-master quorum must persist raft state across reboots |
| `volume.max_volumes` | `0`/auto (binary `-max` default `"8"`) | explicit per-disk required | cloud-init auto-detect is fragile; explicit is predictable |
| all secret lengths | chart placeholders / 10-char JWT | hardened (see §6) | chart placeholders are weak/known |
| `enable_security` (HA example) | `false` | `true` | secure-by-default flagship example |
| S3 `enable_auth` (HA example) | `false` | `true` | secure-by-default flagship example |
| CA/cert key algo | RSA-2048 | ECDSA P-256 (RSA-3072+ opt-in) | modern guidance; hot-reload makes short certs cheap |

### Validation
Use `validation{}` for single-variable constraints; move **cross-variable / cross-object** constraints into module-level `precondition`/`check` blocks or `tftest` assertions (a single variable's `validation` cannot see other objects, and co-located-port collision is inherently cross-object):
- Master quorum: `length(keys(var.master.nodes))` is odd and ≥ 1.
- Replication strings match `^[0-2]{3}$` (XYZ: datacenter/rack/server, each 0–2).
- Every port in `[1,65535]`.
- **Port-collision (all-in-one / co-located roles):** a module-level `check`/`precondition` over the fully-resolved port set per VM — not a per-variable validation. This is the only place metrics-9327 collision can actually be caught.
- `volume.data_dirs` non-empty; each `max_volumes >= 0`; exactly one filer metadata backend enabled.
- Raw escape hatches: validate what's checkable — `can(jsondecode(...))` for JSON, a TOML-parse probe for `extra_config_toml` — and **document that `extra_config_toml`/`extra_args` bypass plan-time checks** (the "plan-time safety" claim does not extend to them).
- Security guardrails: error if `admin.enabled && admin.password == "" && !allow_insecure`; error if client-facing S3 `enable_auth=false && !allow_insecure`; warn if `enable_security=true` but no `allowed_commonNames`/`allowed_wildcard_domain` set.

### Sensitive handling and the state truth
Mark `sensitive = true` on all secrets and on the secret-bearing rendered-config outputs. **State truth, stated plainly:** `sensitive=true` keeps values out of CLI output but **NOT out of state** — every `random_*`/`tls_*` secret persists in state in plaintext regardless. The architectural consequence (§6): **prefer generating secrets outside Terraform** (cloud secret manager / Vault / boot-time generation) so plaintext never enters state where avoidable. On TF 1.10+/OpenTofu, use `ephemeral = true` / write-only args for **supplied** secrets — but see §10 for the version-floor constraint (these cannot leak into the published module's `variables.tf` if we keep a low floor).

### Full per-component surface to expose
The variables must cover at minimum:

| Component | Must-expose variables (flags/env) |
|---|---|
| master | `nodes{}` (keyed), `port` 9333, `grpc_port` 19333, `metrics_port` 9327 (gated on monitoring), `metrics_ip`, `ip_bind`, `mdir`→`-mdir` (`/data`), `log_dir`→`-logdir`, `default_replication`, `volume_size_limit_mb`, `volume_preallocate` (emit-if-true), `garbage_threshold` (null→omit), `disable_http` (emit-if-true), `resume_state`, `raft_hashicorp` (default false), `election_timeout`, `heartbeat_interval`, `metrics_interval_sec`, `volume_growth_copy_{1,2,3,other}`, `extra_args`, `extra_config_toml`, `log_level`, `secret_extra_environment_vars` |
| volume | `node_groups{}`/`volume_groups{}`, `port` 8080, `grpc_port` 18080, `metrics_port`, `metrics_ip`, `ip_bind`, `data_dirs[]{name,path,size,device_name,max_volumes}`→`-dir`/`-max` (binary `-max` default **8**), `idx`→`-dir.idx`, `rack`, `data_center`, `id`, `read_mode` proxy/redirect, `compaction_mbps` 50, `index` memory/leveldb/leveldbMedium/leveldbLarge, `file_size_limit_mb`, `min_free_space_percent` 1, `white_list`, `images_fix_orientation`, `use_rust` + `rust_log`, `public_url`, `static_ip` per node, `extra_args` (NO `pulse_seconds` — removed from the binary) |
| filer | `nodes{}` (keyed), `port` 8888, `grpc_port` 18888, `metrics_port`, `data_dir`, `log_dir`, `default_replica_placement`, `redirect_on_read`, `disable_http`, `disable_dir_listing`, `dir_list_limit` 100000, `max_mb` 32, `encrypt_volume_data`, `rack`, `data_center`, `filer_group`, `recursive_delete` (false), `buckets_folder` /buckets, `backend_type` leveldb2(default-enabled)/mysql/postgres + `WEED_MYSQL_*`/`WEED_POSTGRES_*`, `s3.{enabled,port,https_port,domain_name,enable_auth,config_json,audit_log_config}`, `notification_config_toml`, `secret_extra_environment_vars` (Postgres user/pass from secret) |
| s3 (standalone) | `enabled`, `port` 8333, `https_port`, `metrics_port`, `domain_name`, `enable_auth`, `config_json`, `audit_log_config_json`, `iceberg_port`→`-port.iceberg`, `tls_cert`, `tls_key`, `cacert_file`, `verify_client_cert`, `filer_address`, `reuse_legacy_secret`/`legacy_secret_name` |
| sftp | `enabled`, `port` 2022, `ssh_private_key` `/etc/sw/seaweedfs_sftp_ssh_private_key`, `host_keys_folder` `/etc/sw/ssh`, `auth_methods` password,publickey,keyboard-interactive, `max_auth_tries` 6, `banner_message`, `login_grace_time` 2m, `client_alive_interval` 5s, `client_alive_count_max` 3, `data_center`, `local_socket`/`bind_address`, `config_json` (full user schema below) |
| admin | `enabled`, `port` 23646, `grpc_port` 33646, `data_dir`, `masters`, `url_prefix`, `enable_auth`, `user`, `password`, `existing_secret`/`user_key`/`pw_key` (BYO), `secret_extra_environment_vars` (e.g. `WEED_ADMIN_OIDC_CLIENT_SECRET`) |
| worker | `admin_server`→`-admin <host>:33646`, `job_types` all/default/heavy/csv, `max_detect` 1, `max_execute` 4, `working_dir` /tmp/seaweedfs-worker, `metrics_port`, `replicas` |
| all-in-one | `enabled`, `data_dir`, `idle_timeout` 30, `data_center`, `rack`, `white_list`, `disable_http` (value form `-disableHttp=<bool>`), `metrics_port` **9324**, plus `master.*`/`volume.*`/`filer.*`/`s3.*`/`sftp.*` sub-knobs that **default-inherit** the top-level `s3.*`/`sftp.*` when null |
| global | `log_level` 1, `master_server` (static override consumed by filer/volume/admin), `enable_security`, `enable_replication`, `replication_placement` (overrides per-component when enabled), `cluster_name` (`WEED_CLUSTER_DEFAULT`, default `sw`), `monitoring.{enabled,gateway_host,gateway_port}`, `allow_insecure` (break-glass) |
| security | `enable_security`, `tls_{ca,master,volume,filer,admin,worker,client}_{cert,key}`, **per-component distinct CN**, `allowed_commonNames`/`allowed_wildcard_domain`, `jwt_signing_key`, `jwt_signing_read_key`, `jwt_filer_signing_key`, `jwt_filer_signing_read_key`, `tls_cert_refresh_interval` |
| hardening | systemd SCC analogs (see §4/§6): `run_as_user`, `no_new_privileges`, `protect_system`, `cap_drop_all`, `read_write_paths`, with an object so users can relax |

`extra_args` (list appended verbatim to `ExecStart`) and `extra_config_toml`/raw-config-JSON (raw blocks) are escape hatches mirroring the chart's `*.extraArgs`/`*.config` — explicitly outside plan-time validation.

### COSI-adjacent capabilities (what is dropped vs preserved)
| Capability | Status on VM track |
|---|---|
| `BucketClaim` CRD lifecycle, K8s API plumbing, `cosi.driverName`, `cosi.bucketClassName`, `existingConfigSecret` | **Dropped** — K8s-API-bound |
| Bucket creation with **disk-type / tiered-storage parameters** (`cosi.bucketClassParameters` e.g. `diskType`), `cosi.region`, `cosi.endpoint`, `cosi.enableAuth` intent | **Preserved** — routed through the bucket-bootstrap path (`s3.bucket.create` + `fs.configure -disk=<type>`) and the future `terraform-provider-seaweedfs`. Tiered-storage parity is NOT lost. |

---

## 4. Component-by-Component Deployment Design

Cluster wiring without StatefulSet DNS is the central problem. The chart relies on stable pod DNS (`master-0.master.ns:9333`) and `-master`/`-peers` lists computed from that. On VMs there is no such DNS, so **Terraform computes stable addresses up front (reserved static private IPs / per-node DNS) and templates them into cloud-init before boot.** The Terraform-computed address map is the source of truth — no gossip/tag auto-join for masters. Reserved static IPs: AWS secondary/private IP or `aws_eip`; GCP `google_compute_address` internal `auto_delete=never`; Azure static private IP. **Stable addressing applies to volume servers too** (see Volume).

### Master (Raft quorum)
- ExecStart: `weed -logdir=/var/log/seaweedfs -v=<level> master -port=9333 -mdir=/data -ip=<self_stable_ip> -ip.bind=0.0.0.0 -peers=<m0:9333,m1:9333,m2:9333> -defaultReplication=<XYZ> -volumeSizeLimitMB=1000` plus **conditionally-emitted** `-raftHashicorp` (only if true), `-electionTimeout=10s`, `-heartbeatInterval=300ms`, `-resumeState` (emit-if-true), `-volumePreallocate` (emit-if-true), `-garbageThreshold=<v>` (only if non-null), `-disableHttp` (emit-if-true). `-metricsPort=9327`/`-metricsIp`/`-metrics.address`/`-metrics.intervalSeconds` are **gated behind `global.monitoring.enabled`**, exactly as the chart does — when monitoring is off, the port is not bound (and the security scanner stays happy).
- Provision exactly the keys in `var.master.nodes` (validated odd: 3 or 5) as **discrete `for_each` instances**, never a free ASG that reassigns IPs, never `count`.

**Raft bootstrap — corrected model (this was the single highest-impact error in the draft):**
- `weed`'s hashicorp path (`weed/server/raft_hashicorp.go`) **auto-bootstraps when the on-disk configuration is empty** (`len(GetConfiguration().Servers)==0`). `-raftBootstrap` is **not required** on genuine first boot.
- `-raftBootstrap` when set does `os.RemoveAll(logs.dat, stable.dat, snapshots)` **before** bootstrapping — it **wipes raft state**. A stray `bootstrap=true` on re-apply, a baked AMI carrying the flag, or a tfvars copy-paste against a live quorum master **erases its raft log and snapshots**, which can cascade to total topology-metadata loss. This is far worse than split-brain.
- **Therefore: do NOT template `-raftBootstrap` into the steady-state path.** Rely on the binary's empty-config auto-bootstrap. The `bootstrap` var, if kept at all, is a one-shot **break-glass recovery** flag: it writes a sentinel consumed-and-deleted by `run-weed-master` on the very first boot only, and the systemd `ExecStartPre` **refuses to pass the flag if `<mdir>/logs.dat` or `snapshots/` exists**. Document loudly that `-raftBootstrap` deletes raft state.
- The legacy (non-hashicorp) path keys off `-resumeState`: with `resume_state=false` (binary default) weed **wipes log/conf/snapshot/state on every boot**. We default `resume_state=true` for any multi-master deployment (a deliberate deviation, documented).
- **Pick ONE raft backend deliberately and pin it.** goraft (default, `raft_hashicorp=false`) and hashicorp raft have entirely different on-disk state and membership semantics and are **not interchangeable across an upgrade**. We default `raft_hashicorp=false` to match the binary/chart; switching backends is a documented migration, not a flag flip.

**Master grow/shrink — concrete, uses the real API (not a `replicas`/`-peers` bump):**
- The hashicorp `updatePeers()` reconcile (flag-list → `AddVoter`/`RemoveServer`) runs **only once**, on the first leader-election after a restart that found existing config. Adding a master without a leadership change means the new node **looks up but never becomes a voter** — quorum math stays at the old size, and operators may believe they tolerate more failures than they do. Dropping a master from `-peers` can leave a **ghost voter** that blocks quorum.
- Runbook: provision the new master instance (new key in `var.master.nodes`), confirm it is up, then run `weed shell -master=<leader> cluster.raft.add -id <httpAddr> -address <grpcAddr> -voter` (and `cluster.raft.remove` **before** destroying one), gated on `cluster.raft.ps` showing expected membership. Drive from a `null_resource`/`remote-exec` ordered after the instance is healthy, **not** from the `-peers` template. Surface `cluster.raft.ps` as post-apply verification. Explicitly warn: editing `nodes` alone does not change raft membership.

**Startup gate (corrected endpoint and threshold):** dependents poll **`/cluster/healthz`** (purpose-built: 200=ready, 503=no-leader, 423=leader child-locked), require it to pass on a **quorum** of masters, **twice consecutively** (mirrors the chart's `successThreshold=2`), before starting volumes/filers. `/cluster/status` returns 200 on a follower that merely *knows* the leader, so it is the wrong gate. Success-criteria check asserts exactly one node reports `IsLeader:true` and the rest agree on the same `Leader`.

### Volume (stateful disk owners)
- ExecStart: `weed -logdir=... -v=<level> volume -port=8080 -dir=/data1,/data2 -max=8,8 -ip=<self_stable_ip> -ip.bind=0.0.0.0 -master=<m0:9333,m1:9333,m2:9333> -dataCenter=<dc> -rack=<rack> -readMode=proxy -compactionMBps=50 -minFreeSpacePercent=1 -index=memory` plus optional `-dir.idx=/idx -fileSizeLimitMB=N -images.fix.orientation -whiteList=... -publicUrl=<addr>`. **The master flag is `-master=` (chart's `masterServerArg` helper) — there is no `-mserver` flag; all `-mserver` references are dropped.** Honor a `global.master_server` override before falling back to the computed master list, mirroring `masterServerArg`.
- Provision as keyed `for_each` fixed-identity instances (or a per-node MIG/ASG of size 1), **not** a recyclable ASG. Each owns persistent, AZ-pinned, re-attachable data disks (§5).
- **Stable network identity is required, not optional.** A file ID resolves through the master to a specific volume server's registered `-ip`/`-publicUrl`; the master also tracks nodes by disk-location UUIDs (`RegisterUuids`/`LocationUuids`) and **rejects duplicate UUIDs**. If a replacement re-attaches the same disks but comes up at a **new IP**, the master sees a new node, cached/in-flight file locations to the old IP fail until re-resolve, and an un-aged old entry can trip the duplicate-UUID guard. So: reserved static private IP (or stable per-node DNS) per volume node used in `-ip`/`-publicUrl`, and a documented replace ordering — old node unregistered (graceful unit stop or master age-out) **before** the replacement re-registers the same disk UUIDs. `volume.public_url` + reserved-IP-per-node is a **production default**, not an afterthought.
- `data_dirs` drives both disk resources and the comma-joined `-dir`/`-max`. `idx` (separate, higher-IOPS disk) adds `-dir.idx=/idx`. **idx consolidation is a one-time migration**, not a per-boot action: new idx writes go straight to `-dir.idx`; the `mv *.idx → /idx` step is only for migrating volumes that previously wrote idx into the data dir.
- `-max` binary default is **8** (the chart sets 7); we require explicit `max_volumes` (deliberate deviation) and document the auto path (`weed volume -max 0`).
- **`pulse_seconds` is removed from the binary and is NOT exposed.** Volume↔master heartbeats are **gRPC streaming** on the master gRPC port (19333), not UDP — relevant for firewall reasoning (that port is already open intra-cluster).
- Rust variant: `use_rust=true` → `/usr/bin/weed-volume`, drop `-logtostderr/-logdir/-v`, set `RUST_LOG` env.
- Topology / named volume groups: map cloud AZ → `-rack` (and AZ → `-dataCenter` across regions). **`var.volume_groups = map(object({...}))`** mirrors the chart's `volumes:` map semantics — each entry deep-merges over the base `var.volume` and overrides `replicas`/`data_dirs`/`rack`/`data_center`/`subnet_ids` per group; when non-empty it **replaces** the singular volume group (the chart's `volume.enabled:false` switch). This expresses the chart's primary multi-DC/heterogeneous layout (different disk sizes/counts/racks per group) and drives per-group instance/disk resources and per-group `-rack`/`-dataCenter`.

### Filer (+ optional external DB)
- ExecStart: `weed filer -port=8888 -ip=<self_stable_ip> -ip.bind=0.0.0.0 -master=<m0:9333,m1:9333,m2:9333> -defaultReplicaPlacement=<XYZ> -dirListLimit=100000 -maxMB=32` plus optional `-redirectOnRead -disableDirListing -encryptVolumeData -rack -dataCenter -filerGroup`. Embedded S3 uses the **filer path** `-s3.config=/etc/sw/seaweedfs_s3_config` (distinct from all-in-one's `/etc/sw/s3/...`). The filer ships a default env block: **`WEED_LEVELDB2_ENABLED=true`** alongside a disabled placeholder MySQL block (`WEED_MYSQL_ENABLED=false`, host `mysql-db-host`), `WEED_FILER_OPTIONS_RECURSIVE_DELETE=false`, `WEED_FILER_BUCKETS_FOLDER=/buckets`. Default to leveldb2-enabled with SQL disabled; **validate exactly one metadata backend is enabled**.

- **HA backend — three patterns, leveldb2-replicated is the DEFAULT HA path (corrected):** each filer runs a `MetaAggregator` (`weed/filer/filer.go`) that subscribes to every peer filer's metadata change log (peers discovered via the master's filer-group registry). So **N filers each with their own local leveldb2 form an eventually-consistent replicated metadata tier with NO external DB.** External SQL is *one* option (a shared store), not the only HA path — and a single DB is itself a SPOF unless made HA.

| Pattern | When | Provisioning & rules |
|---|---|---|
| **leveldb2-replicated (default HA)** | most HA deployments | N filers, each with its own durable, re-attachable disk + local leveldb2, replicating via `MetaAggregator`. Async log replication, per-filer catch-up on join, **no quorum**. Hard rule: every filer needs persistent re-attachable storage; losing one filer disk loses only that replica; losing **all** loses metadata. |
| `leveldb2` single | single filer, dev | `WEED_LEVELDB2_ENABLED=true`; persistent disk `/var/lib/seaweedfs/filer`. Not HA. |
| `mysql`/`mariadb`/`memsql`/`postgres` (shared store) | shops that explicitly want a shared SQL store | external **HA** DB (Multi-AZ RDS/Aurora/Cloud SQL/Azure DB — single DB is a SPOF). `WEED_MYSQL_ENABLED=true`, `WEED_MYSQL_HOSTNAME/PORT(3306)/DATABASE(sw_database)`, pooling `MAX_IDLE=5/MAX_OPEN=75/MAX_LIFETIME_SECONDS=600/INTERPOLATEPARAMS=true`; or `WEED_POSTGRES_*`. |

- **DB schema seeding (when SQL chosen) — moved to the boot path, not a cross-host provisioner:** `depends_on` orders **resource creation**, not the moment a remote filer **process** starts on a different host, so a Terraform `remote-exec` seed does not reliably precede the filer. And interpolating the DB password into a provisioner command line exposes it in `ps`/`TF_LOG`/state. Instead: **`run-weed-filer`'s `ExecStartPre` blocks on a real DB-connectivity-and-table-exists probe (retry+backoff)** and runs `CREATE TABLE IF NOT EXISTS` once (credentials fetched from the secret store, never on a command line). Parameterize DDL **per backend** (MySQL `filemeta(dirhash BIGINT, name VARCHAR(766), directory TEXT, meta LONGBLOB, PRIMARY KEY(dirhash,name)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin` vs the Postgres type/collation variant — the MySQL DDL **fails on Postgres**). Prefer the filer's own per-backend init path / documented schema for the exact `weed` version over a hand-maintained DDL, and prefer **IAM/managed DB auth** (RDS IAM / Cloud SQL IAM / Azure AD) so no long-lived password exists in state. Do not auto-create the database itself.

### S3 gateway
- Standalone: `weed s3 -ip.bind=0.0.0.0 -port=8333 -filer=<filer:8888> -config=/etc/sw/seaweedfs_s3_config -auditLogConfig=/etc/sw/s3_auditLogConfig.json` plus optional `-port.https=N -domainName=... -port.iceberg=N -cert.file -key.file -cacert.file` and the client-cert-required mode. `-metricsPort` gated on monitoring as for master.
- **S3 admin credential via environment, not the identity JSON:** `weed/s3api/auth_credentials.go` reads `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` and registers a static **Admin** identity, merged with the JSON config. Deliver the high-value admin secret in the 0600 systemd `EnvironmentFile` (fetched at boot from the secret store) — **never** rendered into the TF-managed identity JSON (which would land in state and metadata), never on the `ExecStart` line. Keep only non-admin/bucket-scoped identities in the JSON if any.
- **Optional S3 data-path mTLS for internal consumers:** expose `cacert_file` + `verify_client_cert` (→ `-cacert.file` + `tls.RequireAndVerifyClientCert`) as defense-in-depth beyond access keys. Keep the public-CA/ACM **server** cert path for external clients. S3 `cert.file`/`key.file` hot-reload via `GetCertificate` (same file-rewrite rotation as gRPC — see §6).
- Stateless → good ASG/MIG candidate behind an L4 LB; roll via Instance Refresh.
- Virtual-host buckets (`{bucket}.{domainName}`): wildcard DNS `*.s3.example.com` → S3 LB; wildcard TLS (`s3.tls_cert`/`s3.tls_key`, or ACM import on AWS). Choose **one** S3-placement pattern (embedded-in-filer vs standalone) per deployment.

### SFTP
- `weed sftp -ip.bind=0.0.0.0 -port=2022 -sshPrivateKey=/etc/sw/seaweedfs_sftp_ssh_private_key -hostKeysFolder=/etc/sw/ssh -authMethods=password,publickey -maxAuthTries=6 -bannerMessage=... -loginGraceTime=2m -clientAliveInterval=5s -clientAliveCountMax=3 -userStoreFile=/etc/sw/seaweedfs_sftp_config -filer=<filer:8888>`.
- **The chart commits a literal ed25519 OpenSSH private host key in `sftp-secret.yaml` — it is world-known. Never copy it.** Generate a **unique** ed25519 host key per deployment (`tls_private_key algorithm=ED25519`) and **fail closed** if none is supplied. (This is one instance of the §6 no-committed-secrets guardrail.)
- **User-store schema (exact):** the user JSON is an array of `{Username, Password, PublicKeys[], HomeDir, Permissions{path:[read|write|list]}, Uid, Gid}` — not generic S3-style identities. Ship the chart's three default users: `admin` (Uid/Gid 0/0, HomeDir `/`, read/write/list), `readonly_user` (1112/1112, read/list), `public_user` (1113/1113, HomeDir `/public`), each with a **generated 20-char password**. Write to `/etc/sw/seaweedfs_sftp_config` (0600) before unit start.

### Admin + Worker
- Admin: `weed admin -port=23646 -port.grpc=33646 -masters=<m0:9333,...> -dataDir=/var/lib/seaweedfs/admin -urlPrefix=...`, with `WEED_ADMIN_USER`/`WEED_ADMIN_PASSWORD` from the `EnvironmentFile`. **Empty password = unauthenticated → require a password** (validation errors when admin enabled and password empty unless `allow_insecure`), and firewall the UI. Support **bring-your-own** credentials via `existing_secret`/`user_key`/`pw_key` (mirrors `admin.secret.existingSecret/userKey/pwKey`), and `secret_extra_environment_vars` for OIDC (`WEED_ADMIN_OIDC_CLIENT_SECRET`).
- Worker: `weed worker -admin=<admin:33646> -jobType=all -maxDetect=1 -maxExecute=4 -workingDir=/tmp/seaweedfs-worker`. No inbound port; dials admin gRPC 33646. Stateless → ASG candidate. Worker unit `After=`/blocks on admin reachability.
- **Admin↔worker mTLS:** the chart provisions distinct **admin-cert** and **worker-cert** (`[grpc.admin]`/`[grpc.worker]`) specifically to secure the admin(33646)↔worker gRPC channel. Both certs must be generated with correct SANs/CNs and present on **both** units' `security.toml`, or workers fail to connect under mTLS.
- These drive vacuum/volume_balance/ec_balance/erasure_coding/iceberg_maintenance. Without them those ops are manual via `weed shell`. Recommend including admin+worker by default for HA and documenting the manual path otherwise.

### All-in-one
- `weed server -dir=/data -master -volume -filer -ip=<self> -ip.bind=0.0.0.0 -idleTimeout=30 -metricsPort=9324 -master.port=9333 -master.defaultReplication=000 -master.volumeSizeLimitMB=1000 -volume.port=8080 -volume.readMode=proxy -volume.compactionMBps=50 -filer.port=8888 -filer.defaultReplicaPlacement=000 -filer.dirListLimit=100000` plus optional `-s3 -s3.port=8333` and `-sftp -sftp.port=2022`. **Metrics on 9324**, not 9327.
- **Config paths for all-in-one are `/etc/sw/s3/seaweedfs_s3_config` and `/etc/sw/s3/s3_auditLogConfig.json`** (distinct from the standalone-filer `/etc/sw/...` path). Wrong path = S3 silently runs unauthenticated or fails auth.
- **Inheritance is load-bearing:** all-in-one s3/sftp sub-fields **default to the top-level `s3.*`/`sftp.*`** when null (chart `-s3.port={{ allInOne.s3.port | default s3.port }}`). `-disableHttp` is emitted in **value form** `-disableHttp=<bool>` for all-in-one.
- Single VM + single systemd unit + single `/data` disk. Gate behind root `topology = "all-in-one" | "distributed"` (mutually exclusive). Multi-replica all-in-one needs ReadWriteMany shared storage and is **not** recommended; default `replicas=1`, document migration to distributed.

### Global replication override (cross-component, was un-wired)
When `global.enable_replication=true`, `global.replication_placement` **overrides** master `-defaultReplication` **and** filer `-defaultReplicaPlacement` **and** the all-in-one `-master.defaultReplication`/`-filer.defaultReplicaPlacement`. Implement the precedence explicitly (global wins when enabled, else per-component value); validate/warn when both global and per-component placement are set. Treating them as independent knobs (the draft's bug) silently diverges data durability from the chart for identical inputs.

### Startup ordering (no StatefulSet controller)
Encode as systemd dependencies + cloud-init gates:
- Master unit: `After=network-online.target` and its mount unit.
- Volume/filer cloud-init: a `runcmd` wait-loop polling `/cluster/healthz` on a **quorum** of masters, **twice consecutively**, with bounded backoff, before `systemctl enable --now`.
- Worker blocks on admin `:23646/health`; s3-standalone blocks on filer. **When `filerRead`/`filerWrite` JWT is on, the filer readiness probe switches from HTTP `/` to a TCP-port probe** (HTTP now needs a token).
- All weed units: `Restart=always`, `RestartSec=5`, `Type=simple`, dedicated non-root `User=seaweedfs`, journald logging, `RequiresMountsFor=/data1 /idx /var/lib/seaweedfs/...` so a missing disk fails the unit loudly. Plus the §6 systemd hardening directives.

---

## 5. Storage & Data Durability

Decouple disks from instances on every cloud so data survives instance replacement:

| Cloud | Disk resource | Attachment | Lifecycle / destroy-protection |
|---|---|---|---|
| AWS | `aws_ebs_volume` (gp3/io2; idx → higher IOPS) | `aws_volume_attachment` | provider v6+ avoids needless instance replacement; `lifecycle { ignore_changes, prevent_destroy=true }`; `delete_on_termination=false`; `disable_api_termination` on instance; **no** `create_before_destroy` on volume nodes |
| GCP | `google_compute_disk` | `google_compute_attached_disk` | `auto_delete=false`; `lifecycle { ignore_changes=[attached_disk], prevent_destroy=true }` on the instance so the two resources don't fight |
| Azure | `azurerm_managed_disk` | `azurerm_virtual_machine_data_disk_attachment` | `for_each` (not `count`); `prevent_destroy=true`; CanNotDelete management lock; the data-disk-attachment recreate-on-change footgun is a named M2 risk with its own test |

**Mandatory destroy-protection (was convention-only, now enforced):** convention ("do not `terraform destroy`") does not survive a 3am on-call engineer or a CI pipeline on the wrong workspace — the most common real-world way these systems lose data. Therefore, **by default**, every stateful disk (volume data/idx, master `-mdir`, leveldb2 filer) is a separate resource with `lifecycle { prevent_destroy = true }`, plus per-cloud cloud-side deletion protection (`delete_on_termination=false`/`disable_api_termination`/`auto_delete=false`/CanNotDelete lock) and instance termination protection on masters/volumes. A documented **break-glass variable** is required to opt out, applied separately. A CI check blocks `terraform plan -destroy` on a stateful module unless the break-glass var is set.

Rules:
- Each data disk must outlive its VM and be re-attachable by a replacement at the **same stable address** (tag-based find-and-attach on boot, or a stable attachment).
- cloud-init `mount-disks.sh`: `blkid`-guard each device — `mkfs.ext4`/XFS **only if unformatted**, then mount by stable path and persist to `/etc/fstab`; never reformat a disk with data. `chown seaweedfs:seaweedfs` the mounts (the OpenShift no-hostPath rule maps to "mounts must be writable by the non-root UID"). idx-consolidation runs only as the one-time migration described in §4.
- Multiple data disks → `-dir d1,d2,... -max m1,m2,...`; disks must have independent I/O paths (separate devices, not partitions) or `-max` is meaningless.
- Master `/mdir` loss = loss of topology metadata — **largely rebuildable from volume-server heartbeats** (slow, not fatal); mdir backup is for *fast* recovery, not the only line of defense.
- Resize: cloud disk expand → online-grow → `growpart`+`resize2fs`. Gate behind an explicit approval variable; never auto-resize on a casual variable change. Shrink unsupported.

**EC durability contract (was missing — the "re-attach disk = no data loss" guarantee does NOT hold universally):** after `ec.encode`, the original `.dat`/`.idx` are deleted and replaced by 14 shards (default 10+4) spread across volume servers; the cluster tolerates losing exactly the parity count of shard-holders. Re-attaching **one** replacement node restores only that node's shards. If a single node held **multiple shards of the same volume** (common on small clusters or before `ec.balance` spreads them), destroying/replacing that node can drop below the recoverable threshold even with "disk preserved" — because the destroyed disk held irreplaceable shards. Therefore:
- EC needs ≥ `ParityShardsCount` (default 4) distinct volume-server nodes; shards must be spread (via `-rack` per AZ + `ec.balance`) so **no single instance/disk holds more than parity-count shards of any volume**.
- The volume drain/replace runbook adds a **hard gate**: before destroy/replace on an EC cluster, verify the target volume's shards remain recoverable without that node (`ec.balance`/shard-location inspection), not merely "disk preserved".

---

## 6. Security & Secrets

**Two non-negotiable principles up front:** (1) the `values.yaml` mirror is **structural only** — every secret default is **hardened**, never copied; (2) secrets must **not transit Terraform values / state / `user_data`** where avoidable — generate them outside TF or at boot.

### Committed-secret guardrail (hard rule + CI gate)
Never embed any private key, password, or signing key as a default in module code or examples. The chart carries world-known placeholder material (literal ed25519 SFTP host key; 10-char JWT keys; single shared CN). A **verbatim mirror habit must not extend to these.** CI greps the entire `terraform/` tree (templates, examples, variable defaults) for `BEGIN ... PRIVATE KEY` and any non-empty default on a `sensitive` variable, and **fails the build** on a match. A checklist of chart defaults that MUST be overridden: SFTP host key, JWT/password lengths, cert CNs, `enableSecurity`, empty admin password.

### CA / cert strategy (cert-manager replacement)
Generate the PKI with the `tls` provider (or, preferred for the CA, Vault PKI — see CA custody):
- Root CA: `tls_private_key` + `tls_self_signed_cert` (`is_ca_certificate=true`). **Default key algo ECDSA P-256** (RSA-3072+ as explicit compatibility opt-in; RSA-2048 is below modern guidance and is *not* the default just because the chart uses it).
- Per component: `tls_private_key` + `tls_cert_request` + `tls_locally_signed_cert`, SANs = each node's stable IP/DNS (computed **per-VM from the address-map input**, not static). **Distinct, predictable CommonName per component** (e.g. `CN=master.seaweedfs.internal`) — **do NOT mirror the chart's single `CN=SeaweedFS CA` for every cert.** Default to **short component certs (24–72h)** paired with automated daily reissue, because reload is automatic (below).

**Peer-identity authorization (was entirely absent — a critical default weakness):** SeaweedFS gRPC mTLS does **not** verify peer identity by default — `AdditionalPeerVerification` returns success unless `grpc.<component>.allowed_commonNames` or `grpc.allowed_wildcard_domain` is set. With the chart's single-CN-for-all certs there is nothing to pin even if you enabled it. Without this, **any holder of any CA-signed cert (including the widely-distributed client cert) can impersonate a master or volume server** — mTLS collapses to "trusted the CA" with no authZ. Fix: distinct CNs (above) **plus** render `grpc.allowed_wildcard_domain` (e.g. `.seaweedfs.internal`) or per-component `allowed_commonNames` so peers are authorized, not just chain-validated. Validation **warns** when `enable_security=true` but no allow-list is set.

**`security.toml` render condition (corrected) and mTLS-vs-JWT separation:** render `security.toml` when `enable_security OR jwt.volume_read OR jwt.filer_write OR jwt.filer_read` — note `volumeWrite` (default true) does **not** trigger it. Render the `[grpc]` TLS block **only** under `enable_security`; a JWT-only config produces a `security.toml` with `[jwt.*]` and no gRPC TLS. **So presence of `security.toml` does NOT imply mTLS.** Generate the CA + 7 component certs (ca/master/volume/filer/admin/client/worker) **only under `enable_security`**, independent of JWT. The admin-cert/worker-cert secure the admin↔worker gRPC (33646) channel and must be on both units.

**Certs DO hot-reload (the draft's central rotation claim was false):** `weed/security/tls.go` loads CA/cert/key via `pemfile.NewProvider` with `RefreshDuration = CredRefreshingInterval` (default 5h, overridable via `WEED_TLS_CERT_REFRESH_INTERVAL`); the S3 HTTPS listener uses `tls.Config{GetCertificate}`, also reload-capable. **Renewal requires NO restart.** Rotation design: write renewed cert/key files **atomically to the same on-disk paths** (`/usr/local/share/ca-certificates/<component>/tls.{crt,key}`); the running process picks them up within the refresh window. Set `WEED_TLS_CERT_REFRESH_INTERVAL` (e.g. `30m`) in the `EnvironmentFile` for faster pickup. Pair short-lived certs with a daily `terraform apply` (or Vault/cron) that rewrites files. **Only a CA roll** requires a coordinated rollout (rewrite `ca/tls.crt` to all nodes, which hot-reloads the root provider). The §12 risk and §8 day-2 sections reflect this: only **JWT-key** changes need a restart.

**CA key custody (was ignored):** the 10y CA **private key** is the crown jewel — whoever holds it mints any component cert and defeats mTLS (especially given the no-peer-verification default above). Generating it with `tls_private_key` puts it in shared state forever, readable by anyone with state read access — a single point of total compromise. Preferred: **generate the CA out-of-band / in a Vault PKI mount** and have Terraform consume an intermediate-issued or short-lived cert (Vault PKI is the natural cert-manager replacement and pairs with hot-reload). If generated in TF, **isolate the CA in a separate, tightly-restricted state**, never co-located with per-cloud infra state. Document a CA-roll runbook (new CA + cross-signed transition). Raised as an open question in §12.

### JWT (`security.toml`)
Four keys: `[jwt.signing]` (volumeWrite, default on), `[jwt.signing.read]`, `[jwt.filer_signing]`, `[jwt.filer_signing.read]`. The chart generates these as `randAlphaNum 10 | b64enc` (~52 bits, base64-wrapped to look longer). **Generate with `random_password` length ≥ 32 (`special=false` for TOML safety) only when the user does not supply their own**, write to the secret store, render `security.toml` from there at boot (0600, owner `seaweedfs`). Validate supplied keys ≥ 32 chars. **JWT-key rotation is NOT hot-reloaded** — it requires a coordinated restart of master+volume (and filer for filer_signing keys); call this out separately from cert rotation. 10s token TTL, no revocation — rotate frequently.

### Secret persistence (get-or-keep — was missing; the draft would rotate creds on every apply)
The chart's `getOrGeneratePassword`/`dig` pattern **preserves** existing keys across upgrades (secrets carry `helm.sh/resource-policy: keep`). The draft's "regenerate stronger keys" has no preservation story — every `terraform apply` would rotate credentials and break running clients. **Replicate keep-existing:** on first apply, write generated secrets to the cloud secret store; on subsequent applies **read them back** and reuse, so re-apply never rotates unless explicitly asked. **Exact lengths (corrected):** S3 `accessKey` 20 / `secretKey` 40; SFTP passwords 20; admin/db per chart; JWT ≥ 32 (hardened).

### Config-file secrets & S3 admin via env
- **S3 admin credential goes in the `EnvironmentFile` as `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`** (fetched at boot), not the identity JSON — keeps the high-value secret out of any TF-rendered artifact. Non-admin/bucket-scoped identities, if any, go in `/etc/sw/seaweedfs_s3_config` (0600): `random_password` access (20) / secret (40), `jsonencode()`d `{identities:[{name,credentials:[{accessKey,secretKey}],actions:[...]}]}`. Actions: `Admin/Read/Write/List/Tagging/Read_ACP/Write_ACP`, optionally bucket-scoped (`Read:bucket1`, wildcard `Read:user-*`). Support `reuse_legacy_secret`/`legacy_secret_name` for migration.
- SFTP user JSON + unique ed25519 host key (0600), written before the unit starts.
- Admin user/password; filer DB creds (prefer IAM DB auth, else secret-store-referenced, never generated-in-state).

### Delivery & the state hazard — secret-store-fetch-at-boot is the DEFAULT, not preference #1
`aws_instance` `user_data`, `google_compute_instance` `metadata.user-data`, and `azurerm` `custom_data` are stored verbatim in state **and** readable from the instance metadata service (169.254.169.254) by any process/SSRF on the box. So **the core renders only NON-secret config into `user_data`.** All secrets (JWT keys, S3 admin secret, DB password, TLS private keys, SFTP host key) are written by Terraform to the cloud secret store (AWS Secrets Manager/SSM SecureString, GCP Secret Manager, Azure Key Vault), and `run-weed-*` **fetches + renders them at boot** using the instance role/MSI:
1. Secret-store-fetch-at-boot (**default & only path** for secret-bearing config). Require **IMDSv2** (`metadata_options http_tokens=required`) and, where feasible, block the metadata CIDR from the weed processes.
2. DB creds and S3 admin secret in the systemd `EnvironmentFile` (`/etc/seaweedfs/weed.env`, 0600, owner `seaweedfs`) — never on `ExecStart` (visible in `ps`/`/proc/*/environ`).
3. Generate secrets in TF only when the user does not supply their own; mark every secret variable and derived output `sensitive` (knowing this hides from CLI, **not** state — hence #1).

### State handling (dedicated requirement — was under-specified)
- **Require** an encrypted, access-restricted, **locking** remote backend: S3 + SSE-KMS + DynamoDB lock (or S3 native locking); GCS + CMEK; `azurerm` + Key-Vault-encrypted storage with blob lease.
- **State truth:** `sensitive=true` hides values from CLI output but **not** from state; every `random_*`/`tls_*` secret persists in state in plaintext. Therefore **prefer generating secrets outside Terraform** (secret manager / Vault dynamic secrets / boot-time generation) and referencing them, so plaintext never enters state. Use `ephemeral`/write-only args (TF 1.10+) for **supplied** secrets where the version floor allows.
- checkov/trivy gate for unencrypted-state and public-state-bucket; document state-file rotation and least-privilege on the backend.

### systemd hardening (OpenShift SCC analog — was entirely missing)
The chart's `openshift-values.yaml` enforces four constraints with direct systemd analogs, exposed via a `hardening` object so users can relax them:

| OpenShift SCC | systemd directive |
|---|---|
| runAsNonRoot / no UID 0 | `User=seaweedfs` (or `DynamicUser=yes`) |
| `allowPrivilegeEscalation=false` | `NoNewPrivileges=true` |
| drop ALL capabilities | `CapabilityBoundingSet=` + `AmbientCapabilities=` (empty) |
| seccomp `RuntimeDefault` | `SystemCallFilter=@system-service` (+ `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`) |
| no hostPath (PVC/emptyDir only) | `ReadWritePaths=` scoped to data/idx/log mounts; mounts `chown`ed to the non-root UID |

Security scanners (checkov/trivy, mandated in CI) will demand these; omitting them makes the VM offering materially less hardened than the chart.

### File-permission/mount contract (cloud-init `write_files` for non-secret; boot-fetch for secret)
CA cert `/usr/local/share/ca-certificates/ca/tls.crt` 0644; component certs 0644; component **keys 0600**; `security.toml` 0600; `seaweedfs_s3_config` 0600; SFTP config/host key 0600. Create the `seaweedfs` user; `chown` accordingly. Secret-bearing files are written by the boot-time fetch script, not baked into `user_data`.

### Node-compromise / decommission runbook (was missing)
No CRL/OCSP exists. When a volume/worker node (and thus its component private key) is compromised or decommissioned, and given the no-peer-verification default, any valid CA-signed cert is fully trusted until CA expiry. Recovery: (a) per-component **distinct CNs + `allowed_commonNames`** so a stolen cert is removed from the allow-list and rejected on next refresh, and/or (b) **roll the CA**. Use **short cert durations** to bound exposure. On destroy, **wipe decommissioned-instance disks and stale secret-store versions** so no orphaned key material survives in detached volumes.

---

## 7. Networking & Observability

### Port matrix (firewall/SG variables)

| Role | HTTP | gRPC | Metrics | Exposure |
|---|---|---|---|---|
| master | 9333 | 19333 | 9327 (gated on monitoring) | intra-cluster; 9333 to clients/admin |
| volume | 8080 | 18080 | 9327 | intra-cluster; **8080 reachable by clients** (direct data fetch). Heartbeats are gRPC streaming on 19333 (master gRPC), not UDP |
| filer | 8888 | 18888 | 9327 | 8888 to clients/LB; 18888 intra |
| s3 | 8333 (+https, +iceberg) | — | 9327 | client-facing via LB |
| sftp | 2022 | — | 9327 | client-facing |
| admin | 23646 | 33646 | 9327 | 23646 to operators; 33646 from workers (mTLS) |
| worker | — | — | 9327 | no inbound |
| all-in-one | 9333/8080/8888/8333/2022 | 19333/18080/18888 | **9324** | mixed |

gRPC = HTTP + 10000 by convention. Provide per-tier `allowed_inbound_cidr_blocks` / `allowed_inbound_security_group_ids` / `allowed_ssh_cidr_blocks`; self-reference the cluster SG for intra-cluster gRPC; expose only s3/filer-HTTP (and sftp) to clients. **Metrics 9327/9324 bind to the private IP (`metrics_ip=<self private>`), never `0.0.0.0`, and are never opened in client-facing SGs** — they leak volume/topology internals. Egress open unless restricted. Every port is a variable so unit, SG, LB, and health check stay consistent.

**Metrics-port collision** (all components default 9327; one VM can bind it once): assign unique metrics ports per co-located role (or rely on all-in-one's 9324) and catch collisions in a **module-level `check`/`precondition`** over the resolved port set — not a per-variable validation (which cannot see across objects). Also recall the port is **gated on `monitoring.enabled`** per the chart.

### Load balancers + health checks
Front **stateless tiers only** — s3 (8333) and optionally filer (8888). Masters/volumes stay directly addressed (clients are redirected to specific volume servers by file ID). Use L4/NLB for the byte-streaming data path. HTTP health checks against the real endpoints:

| Role | Health path | Port |
|---|---|---|
| master | `/cluster/healthz` (liveness; 200/503/423) | 9333 |
| volume | `/healthz` | 8080 |
| filer | `/` (or TCP probe when filer JWT on) | 8888 |
| s3 | `/status` | 8333 |
| admin | `/health` | 23646 |
| worker | `/health`, `/ready` | 9327 |

Map K8s probe timings to LB checks: interval ≈ `periodSeconds` (30 master / 15 volume-readiness / 60 s3), timeout 5–10s, unhealthy_threshold ≈ `failThreshold` (4–5), and emulate master `successThreshold=2` where the platform allows. Prefer HTTP over NLB default TCP-only. Use connection draining so traffic shifts only after readiness. Emit LB DNS as a module output.

### Metrics & dashboards
- **Pull/scrape over push** for VMs: external Prometheus scrapes `:9327/metrics` (`:9324` all-in-one) per instance. Optional push: `-metrics.address=<gw>:<port> -metrics.intervalSeconds=15` when `global.monitoring.gateway_host/port` set — and remember the metrics flags themselves are gated on `monitoring.enabled`.
- Provide service-discovery output (list of `{host, port}` targets) for the operator's Prometheus, or generate `/etc/prometheus/scrape.d/seaweedfs.yml`.
- **Alert on the read-only / low-free-space watermark:** a volume node crossing `minFreeSpacePercent` (default 1%) silently flips all its volumes to **read-only** — this looks like a write outage but is a **capacity event**, distinct from node failure. Surface it from volume metrics + the master's `ReadOnly` state.
- Reuse the chart's bundled Grafana dashboard (`k8s/charts/seaweedfs/dashboards/`, gnetId 10423) as a static file in `examples/`; substitute `${DS_PROMETHEUS}` at apply time.

---

## 8. Day-2 Operations

- **Rolling upgrades (no StatefulSet controller):**
  - Stateless tiers (s3, worker): launch templates + ASG/MIG **Instance Refresh** (`strategy=Rolling`, `min_healthy_percentage` 90%, `instance_warmup`) behind the LB. Launch templates, not legacy launch configs.
  - Masters: controlled, one-at-a-time runbook — replace/upgrade one instance (re-attaching its `-mdir` disk at the **same static address**), wait for `/cluster/healthz` to report ready on a quorum and `cluster.raft.ps` to show the node rejoined, then the next. **Never blind-refresh masters.** The `updatePartition` chart knob has no TF analog; emulate with staged manual steps.
  - Volume servers: one-at-a-time, drain-aware — **quiesce maintenance first** (pause/await any worker EC/vacuum/balance touching the target node's volumes), check the **EC recoverability gate** (§5), stop the unit, replace while re-attaching the same data disks at the same address, wait healthy, next.
- **Scaling masters:** use the real raft API (`cluster.raft.add`/`remove`, verified via `cluster.raft.ps`) per §4 — **not** a `nodes`/`-peers` bump.
- **Scaling volume servers:** add new keyed fixed-identity instances with their own disks, correct `-rack`/`-dataCenter`; the master auto-registers. Removing requires draining/rebalancing data off first. Never destroy a volume instance without disk-preservation **and** the EC recoverability check.
- **Maintenance interactions:** vacuum commit is **all-replicas-or-nothing** — a flapping replica blocks reclamation; `garbageThreshold` gates it. Don't replace/drain a node mid-vacuum/EC (half-compacted state risk). Long jobs need `worker.max_execute` headroom and a maintenance window.
- **Cert rotation: no restart** (hot-reload, §6) — rewrite files; only **CA roll** and **JWT-key** changes need coordinated restarts.
- **Backup/DR — engineered for consistency, not raw file copies:**
  - **Master `-mdir`** (hashicorp = boltdb `logs.dat`/`stable.dat` + `FileSnapshotStore`): a cloud block-snapshot of an open boltdb under active raft writes is **not guaranteed consistent**, and restoring a stale raft snapshot into a live quorum is itself a split-brain/rollback hazard. Prefer the raft snapshot mechanism or quiesce/leader-transfer before snapshotting. Topology is **largely rebuildable from volume heartbeats** if mdir is lost — mdir backup is for *fast* recovery, not the only defense.
  - **Filer:** use SeaweedFS's own metadata tooling (`weed filer.meta.backup`/`filer.backup`) — **not** raw leveldb file copies (leveldb is unsafe to copy file-by-file while open). Snapshot the leveldb2 disk only from a quiesced/snapshot-consistent point. For leveldb2-replicated filers, document the metadata-log catch-up window on a fresh filer and the all-replicas-lost case. For external SQL filers, enable managed automated backups.
  - State concrete **RPO/RTO per tier**, and include an actual **restore-and-verify drill** in the test plan (Terratest currently proves only boot + single-node replace — add a metadata-tier restore).
- **Bucket bootstrapping — port the chart hook verbatim (idempotent, was under-specified):** the chart hook fires on **every** upgrade and is idempotent by design. Per bucket: **list-and-skip guard** (`s3.bucket.list | grep -Fxq <name>` before create), `s3.bucket.create --name X [--withLock]` (lock requested **inline** at create), then `s3.bucket.lock -name X -enable`, `s3.bucket.versioning -name X -status <normalized>` (**normalize** bool/string: `true`/`Enabled`/`enable` → `Enabled`; `false`/`Suspended`/`disable` → `Suspended`), `fs.configure -locationPrefix=<bucketsFolder>/X/ -ttl=<ttl> [-disk=<type>] -apply` (locationPrefix driven by `WEED_FILER_BUCKETS_FOLDER`, default `/buckets`; `-disk` carries the COSI tiered-storage intent), `s3.configure --user anonymous --buckets X --actions Read --apply true` for anonymous read. Expose `create_buckets = [{name, anonymous_read, ttl, object_lock, versioning, disk_type}]` and a `buckets_folder` var. Gate on the master `/cluster/healthz` + filer readiness poll. Long-term: steer to `terraform-provider-seaweedfs` for reconciled (not fire-once) management.

---

## 9. Phased Delivery Roadmap

Two cross-cutting decisions gate everything downstream and are therefore proven in **M0**, not assumed: (a) **keyed `for_each` stateful tiers** with a TF-computed `-peers`/`-master` list and clean middle-node replace; (b) the **address-as-input one-way core boundary**. The original M0 (single all-in-one node) exercised neither — so M0 gains a 3-node master spike.

| Milestone | Scope | Key deliverables | Exit criteria | Rough effort |
|---|---|---|---|---|
| **M0** | AWS PoC + architecture spikes | (1) all-in-one: core skeleton (cloud-init + `weed-server.service` + `/data` mount), `examples/aws-all-in-one`, one `aws_instance` + one protected `aws_ebs_volume`, SG; (2) **3-node master spike**: `for_each`-keyed masters, static IPs as a **core INPUT**, TF-computed `-peers`, auto-bootstrap (no `-raftBootstrap`), `/cluster/healthz` gate | `apply` boots `weed server`; **authenticated** S3 round-trip; destroy/recreate re-attaches `/data`; **middle-master-key replace touches ONLY that node**; quorum elects a leader via healthz | 2–3 wks |
| **M1** | AWS HA distributed (secure-by-default) | core renders master/volume/filer/s3 units; 3-master quorum via auto-bootstrap + healthz gate; volume disks decoupled + `prevent_destroy` + stable per-node IPs + AZ→rack + `volume_groups`; **leveldb2-replicated filer HA (default)** + optional external-DB (boot-path schema seed); s3 standalone + admin-via-env + virtual-host DNS + TLS; admin+worker + admin↔worker mTLS; LBs + healthz checks; mTLS w/ distinct CNs + allow-list + JWT via `tls`/`random` w/ get-or-keep; secrets in secret store + boot-fetch + IMDSv2; systemd hardening; startup-ordering gates; **apply-level Terratest in this milestone** (quorum, register, HA filer, S3 round-trip, single-node replace preserves data, EC recoverability gate) | quorum elects one leader; volumes register at stable addrs; filer HA (leveldb2-replicated) survives one filer loss; authenticated S3 via LB; single-volume replace keeps data; one-at-a-time master upgrade via `cluster.raft.*` keeps quorum; re-apply does NOT rotate secrets | 6–8 wks |
| **M2** | GCP + Azure parity | thin wrappers reusing the shared core; provider-specific disks/IPs/LBs/secret stores/IAM-MSI; named risks: GCP `attached_disk` fighting, Azure data-disk-attachment recreate | AWS HA example reproduced on each; **core unchanged**; **each cloud independently passes single-node-replace-preserves-data Terratest** | **4–6 wks per cloud** (parallelizable) |
| **M3** | K8s-native track (optional) | `terraform-seaweedfs-kubernetes` raw-manifest module reusing config renderers | renders StatefulSets/Services/Secrets; deferred until VM track stable | 2–3 wks |
| **M4** | Registry publish + hardening | public `terraform-<cloud>-seaweedfs` repos; semver tags; per-repo **LICENSE+NOTICE**; runnable `examples/` with real provider blocks; `terraform-docs`; `terraform test` + Terratest in CI; OpenTofu validation under floor+latest; weed-version + core compat matrices; committed-secret scanner; Packer baked-image option; README presence decided per submodule per repo (core README in monorepo, stripped in wrappers to keep internal) | modules indexed on both registries; CI green; nightly Terratest boots a real cluster | 2–3 wks |
| **P (parallel track)** | Data-plane provider | `terraform-provider-seaweedfs` v0.1 = `seaweedfs_s3_bucket` + `seaweedfs_s3_identity` only, on `terraform-plugin-framework`, with import/Read-drift/acceptance harness/GPG-signed goreleaser release; `iam_policy`/`filer_path` deferred | own owner + explicit go/no-go gate; acceptance tests pass against a throwaway all-in-one; **no VM milestone gates on it** | 8–12 wks |

M1 is the bulk of the risk (raft auto-bootstrap semantics, disk re-attach + stable addressing, HA filer model, secret get-or-keep, startup ordering). M2 is **not** "mostly mechanical" — per-cloud disk-lifecycle differences are correctness/data-loss issues, hence 4–6 wks each with their own data-preservation tests. The provider is a permanent maintenance surface on a partly-unstable API contract — split out, owner-gated, never on the critical path.

---

## 10. Testing & CI

- **Static gates (every PR):** `terraform fmt -check`, `terraform validate` (per root + per example), `tflint` with the cloud plugin, security scanner (**trivy** — tfsec folded in — and/or **checkov**: `0.0.0.0/0`, unencrypted disks, **unencrypted/public state bucket**, open metrics ports, missing IMDSv2), `terraform-docs` drift check, and the **committed-secret/weak-default scanner** (BEGIN PRIVATE KEY; non-empty `sensitive` defaults; chart-placeholder matches). Wire via `antonbabenko/pre-commit-terraform`, mirrored in GitHub Actions so local == CI.
- **Native `terraform test` (`*.tftest.hcl`, plan-level, no cloud creds via `mock_provider`):** assert computed artifacts — `-peers`/`-master` list correct for N masters, rendered `ExecStart` flags (including conditional/gated flags) match, `security.toml`/S3 JSON render with the corrected conditions, port-collision `check` fires, replication-string validation fires, global-replication override precedence resolves, **and the index-shift guard (removing a middle stateful key plans destroy of only that node).** These are string/plan outputs; treat them as cheap coverage, **not** as coverage for stateful behavior.
- **Terratest (Go, apply-level) — front-loaded into M1, funded across M1/M2, not an M4 afterthought:** on a real single-cloud lane gated on the HA example, exercise the behaviors only observable at apply time: **raft auto-bootstrap idempotency** (re-apply does not wipe/rotate), **disk re-attach on volume-node replace at the same address**, **DB-seed boot ordering**, master one-at-a-time upgrade via `cluster.raft.*`, authenticated S3 round-trip, single-node-replace-preserves-data, and a **metadata-tier restore-and-verify** drill. Each cloud independently runs the single-node-replace test in M2.
- **OpenTofu compat:** run `validate`/`test` under both `terraform` and `tofu`, at the declared `required_version` **floor and latest**. See the version-floor constraint below.
- **Ephemeral smoke:** one-node all-in-one in CI, run the bucket-bootstrap idempotent path twice (proves the list-and-skip guard), tear down.

**Version-floor honesty (was internally contradictory):** you cannot target a `1.5–1.6` **language** baseline in shipped module code while using `1.9`-cross-variable-validation or `1.10`-ephemeral in that same code — consumers on the floor get parse/validate errors. Decide a **single honest `required_version` floor for shipped modules**:
- If we want per-block/cross-variable validation ergonomics → floor **≥ 1.9**; for `ephemeral` write-only secret inputs → floor **≥ 1.10**.
- If we insist on a low (`1.6`) floor → keep cross-variable validation and `ephemeral` **out of the published `variables.tf`** and enforce those constraints in examples/CI instead.
- `mock_provider` (1.7+) is **test-only** and gated by CI's own TF version — fine regardless of the module floor.
- State the **OpenTofu floor separately** (feature parity differs version-for-version). Add a `versions.tf` matrix to the success criteria; required-providers constraints (e.g. AWS `>= 6.0` for the EBS-attachment behavior) must be declared and load-bearing-tested, not assumed.

---

## 11. Documentation, Examples, Registry & Maintenance

- **Standard Module Structure (hard Registry requirement):** repo root = `main.tf`/`variables.tf`/`outputs.tf`/`versions.tf` + `README.md` + `LICENSE`; runnable `examples/<name>/` each with its own `README.md` **and real `provider`/`required_providers` blocks** (the registry validates examples); composable pieces under `modules/<name>/`. Gotcha: a `modules/` submodule is treated as **public** only if it has a `README.md` — so the core keeps a README in the monorepo (for devs) but is **stripped/omitted in the wrappers** to keep it internal. Every variable/output gets a 1–2 sentence `description`. Group resources into purpose files (`network.tf`, `instances.tf`, `disks.tf`, `loadbalancer.tf`, `config.tf`).
- **Module vs provider publishing are different (do not conflate):** modules need **no** signing; the **provider** repo needs a **GPG key registered with the registry + a goreleaser release pipeline**. Keep the provider's publishing requirements in their own subsection.
- **Per-repo legal:** each wrapper repo carved from the monorepo needs its own **LICENSE + NOTICE** (monorepo license vs standalone-repo license).
- **Examples:** `aws-all-in-one`, `aws-ha-distributed` (secure-by-default), `aws-s3-only`, plus GCP/Azure HA. Each README shows endpoints and a smoke test. No example carries committed secret material.
- **Publishing:** one public repo per cloud named exactly `terraform-<provider>-seaweedfs`; first publish needs a semver tag; new versions ship by pushing a new tag (~1 min webhook). The GitHub repo **description** becomes the registry one-liner — write a real one. Same tags feed `registry.opentofu.org`.
- **Parity without re-wrapping (structural mirror only):**
  - Maintain a **weed-version compatibility matrix** *and* a **wrapper→core→weed** triple table in each README.
  - Keep the canonical core in this monorepo (`terraform/modules/core/`) so a flag/port change in `weed` is updated alongside the code that emits it; distribute it as a **version-pinned registry module** (preferred) or a **CI-enforced** vendored mirror (§2) — never a silently-drifting subtree.
  - When the chart's `values.yaml` adds a knob, add the matching object-field in the core's `variables.tf` in the same change. The **CI flag-diff check (built FIRST, used to author the schema)** diffs the binary's `weed <subcmd> -h` flag set against the core's rendered flags and **fails on drift** — this is also how flag defaults stay re-derived-from-binary rather than copied-from-chart.
  - Independent semver for the modules; do not pin module version == chart version; document which chart era and which `weed` versions each module aligns with.

---

## 12. Risks, Open Questions, Decisions to Confirm

### Top risks (resolved in-plan; listed for visibility)
1. **`-raftBootstrap` wipes raft state.** It `os.RemoveAll`s logs/snapshots before bootstrapping; a stray re-apply or baked flag against a live quorum master causes topology-metadata loss. Resolved: **do not template the flag**; rely on empty-config auto-bootstrap; `bootstrap` survives only as a sentinel-guarded, `ExecStartPre`-refused break-glass flag. (§4)
2. **`count`-index shift on stateful tiers.** Resolved by **keyed `for_each`** API for master/volume/filer, with an index-shift `tftest`. (§2)
3. **Stateful disk loss on replace/destroy.** Resolved by **`prevent_destroy` + cloud-side deletion protection by default** (break-glass to opt out) + decoupled disks + find-and-attach. (§5)
4. **Volume network-identity instability.** Resolved by **stable per-node static IP/DNS** in `-ip`/`-publicUrl` + unregister-before-re-register ordering (duplicate-UUID guard). (§4)
5. **EC durability ≠ disk preservation.** Resolved by the **EC recoverability gate** before any EC-cluster node replace. (§5)
6. **Master raft membership.** Editing `nodes`/`-peers` does **not** change voters; resolved by the **`cluster.raft.add/remove/ps`** runbook. (§4)
7. **mTLS has no peer-identity authZ by default; chart uses one CN for all.** Resolved by **distinct per-component CNs + `allowed_commonNames`/wildcard**. (§6)
8. **Secrets in state / `user_data` / metadata.** Resolved by **secret-store-fetch-at-boot as default**, IMDSv2, EnvironmentFile, encrypted+locking state, get-or-keep persistence, `sensitive`/`ephemeral`. (§6)
9. **Filer DB seed ordering across hosts.** Resolved by **boot-path `ExecStartPre` probe-and-seed**, per-backend DDL, IAM DB auth preferred — not a cross-host provisioner. (§4)
10. **Backup consistency.** Resolved by raft-snapshot/quiesce for mdir and `weed filer.meta.backup` for filer (not raw leveldb copies), with RPO/RTO + restore drill. (§8)
11. **Certs hot-reload (no restart); only JWT-key/CA-roll need restarts/coordination.** (§6/§8)

### Open questions / decisions for maintainers (genuine forks, not silently picked)
- **Raft backend (goraft vs hashicorp):** we default `raft_hashicorp=false` (binary/chart default) and pin one backend, since on-disk state and membership semantics are not interchangeable across a switch. **Confirm the default and whether hashicorp should ever be the supported HA path** (its membership reconcile quirk requires the `cluster.raft.*` runbook regardless).
- **CA key custody:** generate the 10y CA private key **in Vault PKI (recommended, pairs with hot-reload)** vs in Terraform but **isolated in a separate restricted state**? The CA key in shared infra state is a single point of total compromise. Confirm the model and the CA-roll runbook owner.
- **Default HA filer backend:** ship **leveldb2-replicated (MetaAggregator) as the default HA path** (no external DB) vs steer HA users to a managed SQL store? Each has distinct consistency/DR characteristics (async-no-quorum vs shared-store-SPOF-unless-HA). Confirm the default the examples present.
- **Repo placement & core distribution:** canonical core in this monorepo (recommended) distributed as a **version-pinned registry module** vs **CI-enforced vendored mirror**? Confirm, and confirm the mirror/release cadence and the `(wrapper, core, weed)` triple policy.
- **`required_version` floor:** pick **one** honest floor for shipped modules — `≥1.9` (cross-variable validation) or `≥1.10` (`ephemeral` write-only secrets) or a low `1.6` floor that **excludes** those features from `variables.tf`. This directly trades ergonomics/secret-handling against registry portability.
- **Native provider ownership & API contract:** does the project commit to owning/maintaining `terraform-provider-seaweedfs` (a permanent maintenance surface on the partly-unstable AWS-IAM-compatible `:8333` + filer `:8888` contract)? The roadmap places it on a **parallel, owner-gated** track precisely so no VM milestone pre-commits to it — confirm scope (S3 bucket+identity v0.1 only) and the go/no-go gate.
- **K8s-native module:** wanted at all, or is the VM track sufficient for the stated audience? Recommend deferring to post-M2.
- **Cloud priority order:** AWS → GCP → Azure assumed. Confirm, and confirm which managed DB is the default **when** SQL is chosen per cloud (RDS/Aurora vs Cloud SQL vs Azure DB), all required Multi-AZ/HA.
- **Image strategy:** runtime install (cloud-init downloads pinned `weed` + checksum) for v1, Packer baked-image as v2 — confirm acceptable, and confirm the canonical download URL/checksum source.
- **Default replication topology:** core defaults stay `000` (chart default); examples drive AZ→rack one-AZ-loss-survivable (e.g. `010`). Confirm examples-drive-it vs HA-example-defaults-to-survivable.
- **Cert algo & lifetime:** default **ECDSA P-256 + short (24–72h) component certs** (hot-reload makes this cheap) vs RSA-2048/90d for chart fidelity? Recommend the hardened default; confirm.
- **Rust volume server:** keep behind `use_rust=false`, experimental. Confirm.