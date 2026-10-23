# Deploying Lissandra on Nomad

`lissandra.nomad.hcl` runs Lissandra as a single Docker task with:

- a **host volume** for its configuration file,
- **Nomad Variables** for secrets, rendered into the task environment,
- **service registration** (Nomad native or Consul) with an HTTP health check on `/healthz`,
- rolling updates gated on health checks, with auto-revert, and a graceful drain on stop,
- the addresses of the **classification engine**, found through service discovery and rendered into a file that Lissandra re-reads, so engine changes never restart it.

`lissandra-engine.nomad.hcl` is optional. It runs the engine (`laya-serve` from [github.com/NandhaKishorM/laya](https://github.com/NandhaKishorM/laya)), which classifies what Lissandra's rules leave uncategorized. Without it, Lissandra classifies with its rules alone. See [step 6](#6-run-the-engine-optional).

Tested with Nomad 2.0.7. Dynamic host volumes need Nomad 1.10 or newer.

## 1. Build and publish the image

The CI workflow pushes `ghcr.io/<owner>/lissandra:<version>` and `:latest`, for linux/amd64 and linux/arm64, when you push a `v*` tag. To build it yourself with `docker buildx`:

```sh
# both platforms under one tag, pushed (Nomad clients pull the one they run)
make docker-push IMAGE=registry.example.com/lissandra VERSION=v0.1.0

# or one platform, loaded into the local daemon, then pushed
make docker IMAGE=registry.example.com/lissandra VERSION=v0.1.0 PLATFORM=linux/amd64
docker push registry.example.com/lissandra:v0.1.0
```

Without `PLATFORM`, `make docker` builds for the machine it runs on, so on an Apple Silicon Mac set `PLATFORM=linux/amd64` for x86 clients. A multi-platform push needs a builder that can export several platforms: Docker Desktop's default builder can; otherwise create one once with `docker buildx create --use`.

If the image is private, give Nomad clients registry credentials (`auth` in the task's `config`, or a Docker credential helper on the clients).

## 2. Create the volume

**Dynamic host volume (recommended):**

```sh
nomad volume create deploy/nomad/lissandra-volume.hcl
```

The built-in `mkdir` plugin creates the directory on one client, owned by uid 65532 (the container's non-root user). Add a `constraint` to `lissandra-volume.hcl` if you want it on a particular node.

**Static host volume:** add this to a client's configuration and restart that client.

```hcl
client {
  host_volume "lissandra-data" {
    path      = "/opt/nomad/volumes/lissandra"
    read_only = false
  }
}
```

```sh
sudo mkdir -p /opt/nomad/volumes/lissandra && sudo chown 65532:65532 /opt/nomad/volumes/lissandra
```

A CSI volume also works. Change the job's `volume` block to `type = "csi"`.

## 3. Store secrets in a Nomad Variable

The job reads `nomad/jobs/lissandra`, which its workload identity can access with no extra ACL policy. **Every key becomes an environment variable.** Create the variable before running the job, because the template waits for it to exist.

```sh
nomad var put nomad/jobs/lissandra \
  LISSANDRA_ADMIN_TOKEN="$(openssl rand -hex 24)" \
  SMTP_PASSWORD='…' \
  SLACK_WEBHOOK_URL='https://hooks.slack.com/services/…' \
  LISSANDRA_ENGINE_API_KEY='…'         # only if you run the engine (step 6)
```

In the UI, refer to these as `${env:SMTP_PASSWORD}`, `${env:SLACK_WEBHOOK_URL}`, and so on. Lissandra's config file then holds only the references. `${file:/path}` reads a file instead, for example one another `template` block renders; see [Secrets](../../README.md#notifiers-and-routes) in the main README for when each is read. When you change the variable, Nomad re-renders the template and restarts the task. Note that `put` replaces *all* items in the variable (add `-force` to overwrite an existing one), so include every key each time. To change one key and keep the others, for example to rotate the admin token, merge it into the existing items:

```sh
nomad var get -out=json nomad/jobs/lissandra | nomad var put -in=json - LISSANDRA_ADMIN_TOKEN="$(openssl rand -hex 24)"
```

To read your admin token back:

```sh
nomad var get -item=LISSANDRA_ADMIN_TOKEN nomad/jobs/lissandra
```

With `LISSANDRA_ADMIN_TOKEN` in the Variable, the setup wizard's Access step and **Settings → Access** only say where the token comes from, which is the recommended setup. A token set in the UI instead lives in `admin-token.json` on Lissandra's volume and is lost with it; while the variable is set, such a token is ignored (see [Admin token](../../README.md#admin-token)).

**Vault instead of Nomad Variables:** give the task a `vault {}` block and swap the template body, for example:

```hcl
data = <<-EOT
  {{ with secret "kv/data/lissandra" }}{{ range $k, $v := .Data.data }}
  {{ $k }}={{ $v | toJSON }}{{ end }}{{ end }}
EOT
```

## 4. Configure and run the job

```sh
cp deploy/nomad/lissandra.vars.example.hcl deploy/nomad/lissandra.vars.hcl   # gitignored
$EDITOR deploy/nomad/lissandra.vars.hcl
nomad job plan -var-file=deploy/nomad/lissandra.vars.hcl deploy/nomad/lissandra.nomad.hcl
nomad job run  -var-file=deploy/nomad/lissandra.vars.hcl deploy/nomad/lissandra.nomad.hcl
```

On a fresh volume Lissandra starts from its defaults, and the first visit to the UI (step 5) opens the [setup wizard](../../README.md#first-run). A volume that already holds a `config.json` keeps it, and the wizard doesn't open by itself.

| Variable | Default | |
| --- | --- | --- |
| `datacenters` | `["*"]` | |
| `namespace` | `default` | If you change it, the variable path stays `nomad/jobs/lissandra` within that namespace. |
| `image` | `ghcr.io/vipinmohan22/lissandra:latest` | Pin a version in production. |
| `volume_name` | `lissandra-data` | Name of the dynamic or static host volume. |
| `public_url` | `""` | External URL, e.g. `https://lissandra.example.com`. |
| `service_provider` | `nomad` | Set to `consul` to register in Consul. |
| `service_tags` | `[]` | Appended to `lissandra`, `logs`, `metrics`. Use it for Traefik, Fabio or other routing tags. |
| `http_port` | `0` | `0` gives a dynamic port; set e.g. `8080` to pin a host port. |
| `cpu` / `memory` | `300` / `384` | Allow about 2 KB of memory per buffered event. |
| `event_buffer` | `5000` | Recent events kept in memory. |
| `log_level` | `info` | |
| `max_import_mb` | `100` | Largest log file upload on the Log files page. |
| `import_memory_mb` | `64` | Memory for browsable events from analyzed log files. |
| `engine_service` | `lissandra-engine` | Service name of the engine. Every registered instance is used. `""` turns discovery off. |
| `engine_url` | `""` | Fixed engine URL(s), comma-separated, e.g. a load balancer in front of the engine. Overrides `engine_service`. Either way the addresses go into the task's `local/engine_urls` (see [step 6](#run-it)). |

## 5. Reach the UI and point senders at it

- **Behind Traefik** (or another load balancer that reads Nomad or Consul services): set `service_tags` as shown in `lissandra.vars.example.hcl`, and set `public_url` to the external URL.
- **Pinned port:** set `http_port = 8080` and browse to `http://<client>:8080`.
- **Quick look:** `nomad service info lissandra` shows the address and port.

Sign in with the admin token from step 3. On a fresh volume the [setup wizard](../../README.md#first-run) opens: it walks through sources, classification, alert channels and who gets notified, and each step can be skipped or set to Lissandra's defaults.

Senders inside the cluster can discover Lissandra through service discovery. For example, a Fluent Bit or Vector job can template the address with `{{ range nomadService "lissandra" }}{{ .Address }}:{{ .Port }}{{ end }}`. Each source's **Setup instructions** dialog shows ready-made curl, Fluent Bit and Vector snippets with its URL and token filled in.

## 6. Run the engine (optional)

The engine is `laya-serve`, a Python/PyTorch decision model served over HTTP. Lissandra asks it about the events its rules leave uncategorized, once per source and message pattern (see [Classification](../../README.md#classification) in the main README). It runs as its own job so it can be sized, scaled and upgraded on its own, and Lissandra keeps classifying with its rules while the engine is down.

### Build and push the image

No image is published, so build one from the upstream Dockerfile at a release tag, and push it to a registry your Nomad clients can pull from:

```sh
docker buildx build --platform linux/amd64 --push -t registry.example.com/lissandra-engine:0.4.2 \
  https://github.com/NandhaKishorM/laya.git#v0.4.2
```

Set `--platform` to what your Nomad clients run. Unlike Lissandra's image, this one installs Python and PyTorch, so building for another architecture (linux/amd64 on an Apple Silicon Mac) runs under emulation and takes much longer; build on a machine of the clients' architecture when you can.

Tag `v0.4.2` is commit `68804629e8ccd9d616d48a40e87de9fabbeae069`. Build from the commit (`…/laya.git#68804629e8ccd9d616d48a40e87de9fabbeae069`) if you don't want a moved tag to change your image.

| Build argument | Default | |
| --- | --- | --- |
| `TORCH_INDEX` | `cpu` | `cu130` installs PyTorch's CUDA 13.0 wheels, for an NVIDIA GPU. Build it as a separate image, e.g. `lissandra-engine:0.4.2-cu130`. |
| `MODELSCOPE_MODEL` | empty | Bakes checkpoints into the image, e.g. `english` or `"english multilingual"`. They are downloaded from modelscope.cn, not huggingface.co, which helps when clients can't reach the Hub. See [baked weights](#model-cache-offline-mode-and-pinning). |
| `MODELSCOPE_REVISION` | `master` | ModelScope revision to bake. |

The image runs as UID/GID 10001, keeps its model cache in `/home/laya/.cache/huggingface`, and its entrypoint reads the API key from `LAYA_API_KEY_FILE`. Its default command is a one-shot quickstart, so the job runs `laya-serve` explicitly.

### Create the cache volume and the API key

```sh
nomad volume create deploy/nomad/lissandra-engine-volume.hcl
nomad var put nomad/jobs/lissandra-engine LAYA_API_KEY="$(openssl rand -hex 32)"
```

The volume keeps downloaded checkpoints across restarts and upgrades. It's owned by 10001. For `count > 1`, run `nomad volume create` once per instance: each run places the volume on a node that doesn't have one yet.

The job renders `LAYA_API_KEY` into a file in the task's secrets directory that only the image's user can read, and passes its path as `LAYA_API_KEY_FILE`. Any other item in `nomad/jobs/lissandra-engine` becomes an environment variable, for example `HF_TOKEN` for a fine-tuned checkpoint in a private Hub repository.

Lissandra needs the same key as `LISSANDRA_ENGINE_API_KEY` in its own variable. This adds it and keeps the other items:

```sh
nomad var get -out=json nomad/jobs/lissandra | nomad var put -in=json - \
  LISSANDRA_ENGINE_API_KEY="$(nomad var get -item=LAYA_API_KEY nomad/jobs/lissandra-engine)"
```

Lissandra's default engine settings read `${env:LISSANDRA_ENGINE_URL}` (in this job, the address file described below) and `${env:LISSANDRA_ENGINE_API_KEY}`, so there is nothing to set in the UI. To rotate the key, update `nomad/jobs/lissandra-engine` first, then `nomad/jobs/lissandra`. Each job restarts with its new value. In the other order, Lissandra's calls are refused until the engine has the new key too; Lissandra resumes them at its next health check once the engine accepts the key (within 30 seconds).

### Run it

```sh
cp deploy/nomad/lissandra-engine.vars.example.hcl deploy/nomad/lissandra-engine.vars.hcl
$EDITOR deploy/nomad/lissandra-engine.vars.hcl
nomad job run -var-file=deploy/nomad/lissandra-engine.vars.hcl deploy/nomad/lissandra-engine.nomad.hcl
nomad job run -var-file=deploy/nomad/lissandra.vars.hcl deploy/nomad/lissandra.nomad.hcl
```

The order doesn't matter. Run both jobs in the same namespace with the same `service_provider`.

Lissandra's job renders the address of every registered `lissandra-engine` instance, as `http://address:port`, comma-separated, into the file `local/engine_urls` in its task directory, with `change_mode = "noop"`, and sets `LISSANDRA_ENGINE_URL_FILE` to that file. `LISSANDRA_ENGINE_URL` itself is unset, so the default `${env:LISSANDRA_ENGINE_URL}` reads the file, and Lissandra re-reads it every 30 seconds. **Engine instances coming and going therefore never restart Lissandra**, whose recent events and delivery history live in memory. Don't put `LISSANDRA_ENGINE_URL` in `nomad/jobs/lissandra`: a set variable wins over the file. Lissandra spreads requests over the instances and fails over between them. While no instance is registered the file is empty and Lissandra runs on rules alone. Cached answers survive the list changing; they are dropped only when the key, the checkpoints or the categories change, or an instance reports a new checkpoint revision.

Nomad's own service registry lists an instance as soon as it starts, before its check passes; Consul lists only instances whose check passes. Either way, Lissandra checks each instance's `/health` every 30 seconds and sends requests to the instances that answered. While none has, connection errors make it back off, and events keep the rules' result; the next passing check ends the backoff.

To check the connection from Lissandra, use **Settings → Engine → Check connection** in the UI, or:

```sh
curl -H "Authorization: Bearer $LISSANDRA_ADMIN_TOKEN" https://lissandra.example.com/api/engine/health
```

`ok` is true when every instance answers, lists its checkpoints (it does so only when Lissandra's key is right) and has the default checkpoint loaded (`model_loaded`). A checkpoint only a source uses doesn't affect `ok`: each endpoint lists the checkpoints in use that it hasn't loaded under `missing`, next to its `loaded` checkpoints, `device` and `revision`; the UI's **Check connection** warns about them. To see the addresses Lissandra was given: `nomad alloc fs <alloc> lissandra/local/engine_urls`.

| Variable | Default | |
| --- | --- | --- |
| `datacenters` | `["*"]` | |
| `namespace` | `default` | Use the lissandra job's namespace, so it finds the engine. The key's variable path is `nomad/jobs/lissandra-engine` in that namespace. |
| `image` | `lissandra-engine:0.4.2` | Your build (see above). |
| `count` | `1` | Instances. Each needs `cores`, `memory` and its own cache volume. |
| `cores` | `4` | Dedicated CPU cores per instance. Also sets `LAYA_THREADS` and `OMP_NUM_THREADS`. |
| `memory` / `memory_max` | `3500` / `4500` | MB. `memory_max` applies only when memory oversubscription is enabled in the scheduler configuration. |
| `gpu` | `false` | `true` requests one `nvidia/gpu` device and sets `LAYA_DEVICE=cuda`. Needs the `cu130` image. |
| `models` | `english` | `LAYA_MODELS`, loaded at startup. Include every checkpoint Lissandra uses, in Settings and in source overrides: Lissandra doesn't send events to a checkpoint the engine hasn't loaded; they keep the rules' result. |
| `default_model` | `english` | `LAYA_DEFAULT_MODEL`. Lissandra always names the checkpoint it wants, so this only matters to other clients. |
| `max_loaded` | `0` | `LAYA_MAX_LOADED`. `0` means one per entry in `models`. The checkpoints in `models` stay loaded whatever this says (laya-serve raises the cap to fit them); it only limits checkpoints loaded on demand, which Lissandra never asks for. |
| `extra_models` | `""` | `LAYA_EXTRA_MODELS`: JSON `{"name": source}` registering fine-tuned checkpoints. |
| `revision` | `7b928d8…` | `LAYA_REVISION`. See [pinning](#model-cache-offline-mode-and-pinning). |
| `offline` | `false` | `HF_HUB_OFFLINE`: use only the cache. |
| `cache_volume` | `lissandra-engine-cache` | Host volume for the model cache. `""` for none. |
| `port` | `0` | `0` gives a dynamic port; set e.g. `8000` to pin a host port. |
| `service_provider` / `service_tags` | `nomad` / `[]` | As in the lissandra job. |
| `log_level` | `warning` | `LAYA_LOG_LEVEL`. `info` logs every request. |
| `startup_minutes` | `10` | How long a starting instance may take to pass its check before it's restarted. A deployment allows 5 minutes more. |

The job also sets `LAYA_HOST=0.0.0.0`, `LAYA_PORT` from the Nomad port, `LAYA_PRELOAD=1`, `LAYA_DEVICE=cpu` or `cuda` (never `auto`, which crashes laya-serve), `TORCH_DISABLE_NATIVE_JIT=1` and `TOKENIZERS_PARALLELISM=false`.

### Sizing

Lissandra calls the engine once per new message pattern, source and checkpoint and caches the answer for 24 hours, so what counts is how many *new* patterns arrive, not how many events. Structured fields count as part of the pattern (ids of requests, traces and users excepted), so logs whose fields carry other per-event values, such as free text, cost more requests.

- **CPU.** With the english checkpoint on 4 cores, one question takes about 0.4–0.6 s; multilingual is a little faster. Lissandra asks one question per pattern (the category), or two when `set_severity` is on and the severity was only guessed, so an instance answers roughly 1–2 new patterns per second. It works on one request at a time and batching gains nothing on CPU, so scale out with `count` rather than adding cores. Lissandra's `max_in_flight` engine setting is per instance (default 1, which matches), so new instances add capacity without changing any setting.
- **Bursts.** When many new patterns arrive at once, after a deploy or during an outage, they queue in Lissandra. Each event waits at most the engine timeout (`timeout_seconds`, 30 s by default), queueing included, and then goes on with the rules' result, so the engine delays an alert by at most that. An event that would run out of time in the queue isn't queued at all (`engine_shed` in `/api/stats`). A busy engine is never treated as failed. `lissandra_engine_skipped_total` counts events that went on without an answer.
- **Memory.** The english checkpoint uses 2.1–2.4 GB resident and peaks near 3 GB while loading. Multilingual uses about 1.9 GB. The default 3500 MB fits one checkpoint; add about 2 GB for each extra checkpoint in `models`.
- **GPU.** On an NVIDIA T4 a question takes about 35 ms, so a GPU instance keeps up with far more new patterns. Set `gpu = true` and use the `cu130` image; `cores = 2` is enough. Keep the memory, since checkpoints load through host memory. The client needs the [NVIDIA device plugin](https://developer.hashicorp.com/nomad/plugins/devices/nvidia) and the NVIDIA container runtime. The docker driver uses that runtime for a task that is given a GPU.
- **Start-up.** The first start downloads the checkpoint (english 843 MB, multilingual 644 MB plus a 34 MB tokenizer) and loads it, which takes 20–60 s plus the download. Later starts from the cache take about 10–25 s. laya-serve starts listening only once its checkpoints are loaded, so its health check passes only then.

### Model cache, offline mode and pinning

- **Cache.** On first load, checkpoints are downloaded from huggingface.co (`convaiinnovations/laya`) into the cache volume. Later starts use the copy on the volume.
- **Offline.** Once a start has filled the cache, set `offline = true` and laya-serve never contacts the Hub. A start then fails rather than downloads if something it needs isn't cached, so after changing `revision`, `models` or `extra_models`, start once with `offline = false`.
- **Pinning.** `revision` is the Hub commit every checkpoint is loaded at. The default, `7b928d828b7b0e022f929d9bd2e44165aa270148`, was `main` of `convaiinnovations/laya` when this job was written. That repository holds english at its root and multilingual and typed-decisions in subfolders, so one commit pins all three. With a pin, an upstream push can't change the model's answers under you. `revision = ""` follows `main`, so each start that can reach the Hub picks up the latest push. The pin applies to every Hub download, including a fine-tuned checkpoint in `extra_models` given as a Hub repository, which doesn't have that commit: leave `revision` empty in that case, or give the checkpoint as a directory.
- **Baked weights.** An image built with `MODELSCOPE_MODEL` carries its checkpoints in the image's own cache directory, and a volume mounted there hides them. Set `cache_volume = ""`, `offline = true`, and `revision = ""` or the snapshot commit the build printed. The baked snapshot is keyed by the ModelScope commit, so the Hub commit isn't found there. Without a volume and without baked weights, every task start downloads the checkpoint again.
- **Fine-tuned checkpoints.** Register them with `extra_models`, e.g. `{"logs": "/home/laya/.cache/huggingface/checkpoints/logs"}` for a directory you copied onto the cache volume (readable by 10001), or `{"logs": "your-org/lissandra-logs"}` for a Hub repository, with `HF_TOKEN` in the engine's variable if it's private. Add the name to `models`, and choose it as the checkpoint in Lissandra's engine settings or on a source.

### Upgrades

Change `image` or `revision` and re-run the job. Instances are replaced one at a time, and the deployment reverts if a new one isn't healthy within `startup_minutes` plus 5 minutes. With `count = 1` the engine is down while the new instance loads, and events are classified by the rules meanwhile. Lissandra keeps running and picks up the new address within 30 seconds. To avoid the gap, add `canary = 1` and `auto_promote = true` to the job's `update` block. That costs a second instance's resources during the deployment.

`kill_timeout` is 60 s so that requests in flight can finish (laya-serve waits for them on SIGTERM), but Nomad clients cap it at their `max_kill_timeout`, which is 30 s by default.

### Engine troubleshooting

| Symptom | Check |
| --- | --- |
| Allocation stuck pending with `Missing: nomad.var.block(nomad/jobs/lissandra-engine…)` | Create the variable (see above). |
| The task exits at once with `Invalid empty or NUL-containing secret in LAYA_API_KEY_FILE` | The variable has no `LAYA_API_KEY` item. |
| The check fails for minutes after a start, then the task restarts | The first start downloads the checkpoint: see `nomad alloc logs -stderr <alloc>`. On a slow link raise `startup_minutes`. With `offline = true` and a cache that doesn't hold the checkpoint at `revision`, loading fails: start once online. |
| `PermissionError` under `/home/laya/.cache/huggingface` | The cache volume must be owned by `10001:10001`. The volume spec does that; a static host volume needs `chown 10001:10001`. |
| Exit code 137, OOM killed | Raise `memory`, by about 2 GB per extra checkpoint in `models`. |
| Lissandra's engine health says `the engine did not list its checkpoints: check the API key` | `/health` answers anyone with `{"status":"ok"}` but lists checkpoints only to callers with the key, so the Nomad check passes either way. Make `LISSANDRA_ENGINE_API_KEY` in `nomad/jobs/lissandra` match `LAYA_API_KEY` in `nomad/jobs/lissandra-engine`. With a wrong key, requests fail with 401 and Lissandra pauses engine calls for 5 minutes. |
| An endpoint has `missing` checkpoints (health is not ok when the default checkpoint is one of them: `model_loaded` is false); the engine status warns `checkpoint not loaded on the engine: "…" (it has …)` | A checkpoint Lissandra uses (in Settings or on a source) isn't in `models`. Lissandra doesn't ask for it, because laya-serve would load it on the first request: a second model in memory, possibly beyond `memory`, evicting another checkpoint at `max_loaded`, or downloading it. Events that need it keep the rules' result. Add it to `models` and raise `memory` by about 2 GB. |
| Lissandra's engine status warns `checkpoint "…" is paused` and shows `asked for "…", got "english"` | laya-serve answers a checkpoint name it doesn't know with another checkpoint, without saying so. Lissandra checks the name in every answer and pauses that checkpoint for 5 minutes; other checkpoints carry on. Fix the name in Settings or on the source, or register it with `extra_models`. |
| `/api/engine/health` reports `"device": "cpu"` with `gpu = true` | laya-serve falls back to CPU silently. The image was built without `TORCH_INDEX=cu130`, or the client lacks the NVIDIA runtime or device plugin. |
| laya-serve exits at start after `LAYA_DEVICE` was changed to `auto` | Use `cpu` or `cuda`. `auto` crashes it. |
| `Failed to find C compiler` on the first GPU request | `TORCH_DISABLE_NATIVE_JIT=1` must be set. The job sets it. |
| `lissandra_engine_skipped_total` rising, `engine_shed` in `/api/stats` growing | The engine can't keep up with new patterns, so events that would wait longer than `timeout_seconds` go on without it. Add instances (`count`) or use a GPU. Raising `max_in_flight` doesn't help: laya-serve still answers one request at a time. |
| `lissandra_engine_paused` is 1, `paused_until` set in the engine status | Three requests in a row ran out of time, or instances are unreachable or failing: Lissandra backs off for 5 s to 2 minutes, and the next passing health check (every 30 s) ends it. A rejected key or a wrong URL path pauses for 5 minutes; `last_error` says which. |

## Analyzing log files through a proxy

Uploads on the **Log files** page are streamed and analyzed while they arrive. A large file can keep one request open for several minutes. Defaults that cut such requests short:

- **Traefik:** `entryPoints.<name>.transport.respondingTimeouts.readTimeout` is 60 s in v3. Raise it, e.g. to `3600s`, on the entrypoint Lissandra uses.
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
- Lissandra reports the engine as `lissandra_engine_*` metrics: calls, errors, timeouts, cache hits, answers applied, answers below the confidence threshold, events skipped, queue depth, requests completed, request and inference seconds, and whether calls are paused ([list](../../README.md#configuration)). `rate(lissandra_engine_request_seconds_total[5m]) / rate(lissandra_engine_requests_completed_total[5m])` is the average round trip, and the same with `lissandra_engine_inference_seconds_total` the engine's compute time; a growing gap between them means requests wait inside the engine. The engine itself has no metrics endpoint; its Nomad check on `/health` covers liveness.

## Upgrades and backups

- **Upgrade:** change `image` and re-run the job. The config file on the volume is kept; settings it doesn't have yet (the engine's, when upgrading from a release before it) start at their defaults. If the new version doesn't pass its health check, Nomad reverts to the old one. When the old one stops, it finishes the events it accepted and delivers queued notifications first (up to 28 s, within the job's 30 s `kill_timeout`). To upgrade the engine, see [Upgrades](#upgrades) in step 6.
- **Backup:** use **Settings → Backup → Export** (masked) in the UI, or copy `config.json` from the volume. Exports carry neither the admin token nor the setup wizard's progress; a token set in the UI is in `admin-token.json` next to `config.json`, so a copy of the volume restores both. Secrets referenced as `${env:…}` live in the Nomad Variable, so back that up too (`nomad var get -out=json nomad/jobs/lissandra`). The engine's cache volume needs no backup: it's downloaded again.
- **Seeding a new cluster:** render a known config into the task and set `LISSANDRA_CONFIG_SEED` to its path. It's used only when `config.json` doesn't exist yet. A seed doesn't open the setup wizard unless it has an `"onboarding": {}` key.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Allocation stuck pending with `Missing: nomad.var.block(nomad/jobs/lissandra…)` | The variable doesn't exist yet. Create it (step 3). |
| `permission denied` writing `/data/config.json` | Volume ownership. Static volumes must be owned by `65532:65532`. |
| Placement fails with `missing compatible host volumes` | The volume name or node doesn't match. Run `nomad volume status -type host` or check the client's `host_volume` config. |
| UI rejects the token | `nomad var get -item=LISSANDRA_ADMIN_TOKEN nomad/jobs/lissandra`. The task restarts after the variable changes. Without the variable, the token is the one set in the UI (`admin-token.json` on the volume) or, if none was, the one Lissandra logged as `admin_token`: `nomad alloc logs <alloc> \| grep admin_token`. |
| Notifications fail | **Deliveries** in the UI shows the HTTP status or SMTP error from the last attempt, and `nomad alloc logs -stderr <alloc>` has details. |
| Lissandra restarts by itself now and then | Check `nomad alloc status <alloc>` for `Template with change_mode restart re-rendered`: the variable `nomad/jobs/lissandra` changed. Engine instances coming and going don't restart Lissandra. |
| Events never show an engine answer | `/api/engine/health` and the `engine` part of `/api/stats` show the URLs Lissandra uses, the last error, warnings and whether calls are paused. The address file is empty while no `lissandra-engine` instance is registered: `nomad service info lissandra-engine`, and `nomad alloc fs <alloc> lissandra/local/engine_urls`. Only events at or above `min_severity` that no rule categorized are sent. See also [Engine troubleshooting](#engine-troubleshooting). |
