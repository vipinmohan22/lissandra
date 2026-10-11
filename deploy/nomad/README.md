# Deploying Laya on Nomad

`laya.nomad.hcl` runs Laya as a single Docker task with:

- a **host volume** for its configuration file,
- **Nomad Variables** for secrets, rendered into the task environment,
- **service registration** (Nomad native or Consul) with an HTTP health check on `/healthz`,
- rolling updates gated on health checks, with auto-revert.

Tested with Nomad 2.0.7. Dynamic host volumes need Nomad 1.10 or newer.

## 1. Build and publish the image

The CI workflow pushes `ghcr.io/<owner>/laya:<version>` and `:latest` when you push a `v*` tag. To build it yourself:

```sh
make docker IMAGE=registry.example.com/laya VERSION=v0.1.0
docker push registry.example.com/laya:v0.1.0
```

If the image is private, give Nomad clients registry credentials (`auth` in the task's `config`, or a Docker credential helper on the clients).

## 2. Create the volume

**Dynamic host volume (recommended):**

```sh
nomad volume create deploy/nomad/laya-volume.hcl
```

The built-in `mkdir` plugin creates the directory on one client, owned by uid 65532 (the container's non-root user). Add a `constraint` to `laya-volume.hcl` if you want it on a particular node.

**Static host volume:** add this to a client's configuration and restart that client.

```hcl
client {
  host_volume "laya-data" {
    path      = "/opt/nomad/volumes/laya"
    read_only = false
  }
}
```

```sh
sudo mkdir -p /opt/nomad/volumes/laya && sudo chown 65532:65532 /opt/nomad/volumes/laya
```

A CSI volume also works. Change the job's `volume` block to `type = "csi"`.

## 3. Store secrets in a Nomad Variable

The job reads `nomad/jobs/laya`, which its workload identity can access with no extra ACL policy. **Every key becomes an environment variable.** Create the variable before running the job, because the template waits for it to exist.

```sh
nomad var put nomad/jobs/laya \
  LAYA_ADMIN_TOKEN="$(openssl rand -hex 24)" \
  SMTP_PASSWORD='…' \
  SLACK_WEBHOOK_URL='https://hooks.slack.com/services/…' \
  ANTHROPIC_API_KEY='sk-ant-…'          # only if you enable AI classification
```

In the UI, refer to these as `${env:SMTP_PASSWORD}`, `${env:SLACK_WEBHOOK_URL}`, and so on. Laya's config file then holds only the references. When you change the variable, Nomad re-renders the template and restarts the task, so rotating the admin token is a single `nomad var put`. Note that `put` replaces *all* items in the variable (add `-force` to overwrite an existing one), so include every key each time.

To read your admin token back:

```sh
nomad var get -item=LAYA_ADMIN_TOKEN nomad/jobs/laya
```

**Vault instead of Nomad Variables:** give the task a `vault {}` block and swap the template body, for example:

```hcl
data = <<-EOT
  {{ with secret "kv/data/laya" }}{{ range $k, $v := .Data.data }}
  {{ $k }}={{ $v | toJSON }}{{ end }}{{ end }}
EOT
```

## 4. Configure and run the job

```sh
cp deploy/nomad/laya.vars.example.hcl deploy/nomad/laya.vars.hcl   # gitignored
$EDITOR deploy/nomad/laya.vars.hcl
nomad job plan -var-file=deploy/nomad/laya.vars.hcl deploy/nomad/laya.nomad.hcl
nomad job run  -var-file=deploy/nomad/laya.vars.hcl deploy/nomad/laya.nomad.hcl
```

| Variable | Default | |
| --- | --- | --- |
| `datacenters` | `["*"]` | |
| `namespace` | `default` | If you change it, the variable path stays `nomad/jobs/laya` within that namespace. |
| `image` | `ghcr.io/vipinmohan22/laya:latest` | Pin a version in production. |
| `volume_name` | `laya-data` | Name of the dynamic or static host volume. |
| `public_url` | `""` | External URL, e.g. `https://laya.example.com`. |
| `service_provider` | `nomad` | Set to `consul` to register in Consul. |
| `service_tags` | `[]` | Appended to `laya`, `logs`, `metrics`. Use it for Traefik, Fabio or other routing tags. |
| `http_port` | `0` | `0` gives a dynamic port; set e.g. `8080` to pin a host port. |
| `cpu` / `memory` | `300` / `256` | Allow about 2 KB of memory per buffered event. |
| `event_buffer` | `5000` | Recent events kept in memory. |
| `log_level` | `info` | |
| `max_import_mb` | `100` | Largest log file upload on the Log files page. |
| `import_memory_mb` | `64` | Memory for browsable events from analyzed log files. |

## 5. Reach the UI and point senders at it

- **Behind Traefik** (or another load balancer that reads Nomad or Consul services): set `service_tags` as shown in `laya.vars.example.hcl`, and set `public_url` to the external URL.
- **Pinned port:** set `http_port = 8080` and browse to `http://<client>:8080`.
- **Quick look:** `nomad service info laya` shows the address and port.

Senders inside the cluster can discover Laya through service discovery. For example, a Fluent Bit or Vector job can template the address with `{{ range nomadService "laya" }}{{ .Address }}:{{ .Port }}{{ end }}`. Each source's **Setup instructions** dialog shows ready-made curl, Fluent Bit and Vector snippets with its URL and token filled in.

## Analyzing log files through a proxy

Uploads on the **Log files** page are streamed and analyzed while they arrive. A large file can keep one request open for several minutes. Defaults that cut such requests short:

- **Traefik:** `entryPoints.<name>.transport.respondingTimeouts.readTimeout` is 60 s in v3. Raise it, e.g. to `3600s`, on the entrypoint Laya uses.
- **nginx:** `client_max_body_size 1m` by default. Set it at least to `max_import_mb`. Add `proxy_request_buffering off` (so analysis starts while uploading) and a long `proxy_read_timeout`.

For files bigger than the upload limit, run the analysis where the file is: `docker run --rm -i <image> analyze - < app.log`. To use your team's rules, export them from **Settings → Backup** and pass `-config`.

## Memory sizing

The task needs roughly:

- ~2 KB per buffered live event (`event_buffer`);
- plus `import_memory_mb` for browsable events from log files;
- plus about 100 MB of working memory while a file is being analyzed.

The job sets `GOMEMLIMIT` to 80% of `memory`, so Go reclaims memory before Docker's limit is reached. The default of 384 MB covers the defaults with room to spare.

## Monitoring

- `/metrics` is Prometheus format, and the service carries the `metrics` tag. With Prometheus's `nomad_sd_configs` (or `consul_sd_configs`), keep targets whose tags include `metrics` (`__meta_nomad_tags` / `__meta_consul_tags`).
- The job's health check uses `/healthz`. `update.health_check = "checks"` means a broken release never becomes healthy and is reverted automatically.

## Upgrades and backups

- **Upgrade:** change `image` and re-run the job. The config file on the volume is kept, and the old version stays until the new one passes its health check.
- **Backup:** use **Settings → Backup → Export** (masked) in the UI, or copy `config.json` from the volume. Secrets referenced as `${env:…}` live in the Nomad Variable, so back that up too (`nomad var get -out=json nomad/jobs/laya`).
- **Seeding a new cluster:** render a known config into the task and set `LAYA_CONFIG_SEED` to its path. It's used only when `config.json` doesn't exist yet.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Allocation stuck pending with `Missing: nomad.var.block(nomad/jobs/laya…)` | The variable doesn't exist yet. Create it (step 3). |
| `permission denied` writing `/data/config.json` | Volume ownership. Static volumes must be owned by `65532:65532`. |
| Placement fails with `missing compatible host volumes` | The volume name or node doesn't match. Run `nomad volume status -type host` or check the client's `host_volume` config. |
| UI rejects the token | `nomad var get -item=LAYA_ADMIN_TOKEN nomad/jobs/laya`. The task restarts after the variable changes. |
| Notifications fail | **Deliveries** in the UI shows the HTTP status or SMTP error from the last attempt, and `nomad alloc logs -stderr <alloc>` has details. |
