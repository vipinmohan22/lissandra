# Laya

Laya reads logs, classifies them, and alerts the right people. It runs as a single small Go service with a built-in web UI, and ships with a Nomad job for deployment.

- **Ingest two ways.** Senders POST logs to Laya's ingest endpoint (push), or Laya polls your HTTP APIs for logs (pull). JSON, JSON arrays, NDJSON and plain text are all accepted.
- **Analyze log files.** Upload a log file on the **Log files** page (or with curl, or `laya analyze` in a terminal) to get a classified report: a timeline in the file's own time, top issues, the alert routes that would have fired, and searchable events. Analysis never touches live events or sends alerts.
- **Classify.** Every event gets a severity, a category and tags. Built-in detection reads level fields and common log formats; an ordered rule engine that you edit in the UI does the rest. Repeats of the same problem are grouped by fingerprint.
- **Learn.** When the Laya engine categorizes a pattern no rule covers, Laya turns the answer into a rule for that pattern, marked for review. From then on the rules classify it (and alert on it) without asking the model. Review learned rules on their own tab, then approve, edit or reject them.
- **Notify.** Routes send matching events to pluggable notifiers: email (SMTP), Slack, Microsoft Teams, Discord, Google Chat, PagerDuty, or any HTTP endpoint through a templated webhook. Per-problem throttling keeps channels quiet.
- **A local model for what the rules miss.** Events the rules leave uncategorized go to the [Laya engine](#laya-engine), a small decision model that runs next to Laya (`make up` starts both), which picks one of your categories. Rules always win, a severity the log states is never replaced, unsure answers are ignored, and each message pattern is asked about once.

```mermaid
flowchart LR
  A[Apps / Fluent Bit / Vector] -- "POST /ingest/{source}" --> I[Ingest]
  P[Pollers] -- "GET/POST your log APIs" --> I
  I --> C[Classifier: heuristics + rules]
  C -. "uncategorized, once per pattern" .-> E[Laya engine: local model]
  E -. "category, severity" .-> C
  C --> S[(Recent events + stats)]
  C --> R{Alert routes}
  R --> N1[Email]
  R --> N2[Slack]
  R --> N3[Teams]
  R --> N4[Discord / Google Chat / PagerDuty / webhook]
  UI[Web UI + REST API] --- S
```

## Quick start

```sh
make up     # Laya and its local model, with Docker Compose
# → http://localhost:8080, sign in with LAYA_ADMIN_TOKEN from .env
make down
```

`make up` writes `.env` with a new admin token and engine key on its first run, then starts two containers from `compose.yaml`: Laya, and the [Laya engine](#laya-engine) with the `english` checkpoint. The first start builds the engine image from upstream (PyTorch, a few minutes) and downloads the checkpoint (about 0.85 GB) into the `laya-model-cache` volume; `make up` returns once the model is loaded. Later starts load it from the volume in seconds. Give Docker at least 6 GB of memory: the engine uses about 2.4 GB, and nearly 3 GB while loading. Nothing leaves the machine except the one-time downloads.

To work on Laya itself (Go 1.24+), run the engine in Docker and Laya on the host:

```sh
make engine   # the model alone, on 127.0.0.1:8000
make run      # Laya, using that engine (it reads .env)
```

Without `.env`, `make run` runs on rules alone. The single image works too: `docker buildx build -t laya --load .` and `docker run -p 8080:8080 -e LAYA_ADMIN_TOKEN=… -e LAYA_ENGINE_URL=http://<engine>:8000 -e LAYA_ENGINE_API_KEY=… -v laya-data:/data laya`.

With `make up`, `.env` sets `LAYA_ADMIN_TOKEN`, so it is the only admin token. Without `LAYA_ADMIN_TOKEN`, Laya generates an admin token for each run and logs it as `admin_token`; sign in with it and the [setup wizard](#first-run) lets you set a permanent one. In production, set `LAYA_ADMIN_TOKEN` from your secret store (on Nomad, a Variable) instead.

The image builds with `docker buildx build` (the legacy `docker build` can't read its Dockerfile), and `docker` needs a running daemon. Docker Desktop has both. On a Mac without it: `brew install colima docker docker-buildx`, add `"cliPluginsExtraDirs": ["/opt/homebrew/lib/docker/cli-plugins"]` to `~/.docker/config.json` so the CLI finds the plugin, then `colima start`. `make docker` builds `ghcr.io/vipinmohan22/laya:<version>` for your machine's platform; `make docker PLATFORM=linux/amd64` builds for x86 servers from an Apple Silicon Mac (Go cross-compiles, so it is as fast as a native build). The container runs as UID 65532: a named volume works as is, a host directory must be writable by that user.

A fresh install has one push source called `default`, and the first sign-in opens the [setup wizard](#first-run). The wizard's Sources step, or **Sources → Setup instructions**, shows the source's ingest URL and token. Send something:

```sh
curl -X POST http://localhost:8080/ingest/default \
  -H "Authorization: Bearer <ingest token>" \
  -H "Content-Type: application/json" \
  -d '{"level":"error","service":"checkout","message":"payment failed: connection refused to db:5432"}'
```

It appears on **Events** classified as `error / network`. Add a notifier under **Notifiers**, use **Send test** to check it, then create an **Alert route** to connect them.

To deploy on Nomad, see [deploy/nomad/README.md](deploy/nomad/README.md). It includes a second job for the [Laya engine](#laya-engine).

## First run

The first time you sign in to a fresh data directory, Laya opens a setup wizard: a welcome page, five steps, and a review of what you set up.

| Step | What you set | Use defaults |
| --- | --- | --- |
| Access | A permanent admin token, generated in your browser and shown before it is saved | Saves the generated token |
| Sources | Laya's public URL, and the sources; it shows when the first event arrives | Adds a push source if none is enabled |
| Classification | Categories and which rules are on, with a tester for your choices | Adds the built-in categories that are missing; rules are never changed |
| Alert channels | Email, Slack, Teams, Discord, Google Chat, PagerDuty or a webhook | No alerts for now |
| Who gets notified | Each channel's recipients (email To, Slack mention) and its route: minimum severity, categories, throttle | A route for each channel nothing routes to yet: error and above (critical for PagerDuty), 15-minute throttle |

Every step can be skipped or set to Laya's defaults. Defaults only add what is missing, and the button says what it will add. **Use defaults for everything** on the welcome page does every step at once but never changes the admin token. Laya has no user accounts: people get alerts through channels, and the review page shows who gets what.

- **Signing in the first time.** Without `LAYA_ADMIN_TOKEN`, use the token Laya logged as `admin_token`, for example `docker logs <container> 2>&1 | grep admin_token`. It changes on every restart until you set a permanent one in the Access step or under **Settings → Access**.
- **Finish later** leaves setup for now, and the dashboard offers to resume it. **Skip setup** ends it and keeps the configuration as it is.
- **Running it again.** Start it from **Settings → Setup guide**. What the wizard saves is ordinary configuration, which the other pages edit too.
- **Which installs get it.** Only a configuration Laya creates from its defaults opens the wizard by itself. An existing install that is upgraded doesn't, and neither does one started from `LAYA_CONFIG_SEED`, unless the seed has an `"onboarding": {}` key. A seed can also mark steps as done: `"onboarding": {"steps": {"sources": {"outcome": "done"}}}`.

## Concepts

### Sources

| Type | How it works |
| --- | --- |
| **push** | Laya serves `POST /ingest/{source id or name}`. Authenticate with `Authorization: Bearer <token>`, an `X-Laya-Token` header, or `?token=`. Bodies may be gzip-encoded. Each source has its own token, which you can rotate. |
| **pull** | Laya calls your endpoint on an interval with optional bearer, basic or custom-header auth, extra headers and a POST body. `{since_rfc3339}`, `{since_unix}`, `{since_unix_ms}`, `{now_rfc3339}`, `{now_unix}` and `{now_unix_ms}` in the URL or body are replaced on every poll ("since" is the previous successful poll), and records already seen can be skipped. **Test fetch** in the editor previews parsed and classified records before you save. |

Parsing is shared by both. Laya finds the message, level and timestamp automatically (`message`/`msg`/`log`/`MESSAGE`…, `level`/`severity`/`PRIORITY`…, `@timestamp`/`timestamp`/`time`/`ts`…, including epoch seconds, milliseconds, microseconds and nanoseconds), unwraps common envelopes (`records`, `logs`, `events`, `data`, `items`, `results`, Elasticsearch `hits.hits[]._source`), and folds indented stack-trace lines into the preceding plain-text line. You can override any of these per source, including a dot-path to the records array (e.g. `data.items`).

### Log files (manual analysis)

Open **Log files**, drop one or more files, and Laya parses and classifies them with your current rules, plus the answers the Laya engine already gave for the same records, numbers and ids aside (to the live events of the source you parse the file as, or to earlier files). Each file gets a report:

- lines, events, errors, and the time span the file covers;
- a timeline in the file's own time;
- counts by category and by matched rule;
- the alert routes that would have fired, and roughly how many alerts that would have meant (nothing is actually sent);
- top issues (patterns), plus an **Uncategorized patterns** list with a **Create rule** shortcut;
- searchable, filterable events with line numbers.

You can download the report (JSON) and the events (NDJSON), and re-run a file after changing rules.

**With the Laya engine.** When the engine is configured, check **Ask the Laya engine about patterns no rule categorizes** (`engine=1` over HTTP) to have it classify the patterns it hasn't answered yet, as it would for live events. That is one request per pattern, at most 100 per file, and only while the engine has a slot no live event needs, so live classification comes first. Patterns it doesn't get to keep the rules' result. The report counts the events the engine answered and the ones its answer changed. Without the option, analysis never calls the engine.

What it understands:

| | |
| --- | --- |
| Formats | Plain text, NDJSON, JSON (arrays, back-to-back objects, and exports that wrap records such as Elasticsearch `hits.hits`, CloudTrail `Records`, or `data`/`events`/`logs`), and CSV with a header row (`.csv`; e.g. Kibana or CloudWatch Insights exports). JSON is streamed one record at a time, so exports of any size work. |
| Multi-line records | Java/logback stack traces including unindented `Caused by:` and exception headers, Python tracebacks, Go panics. When most lines start with a timestamp, a record runs until the next timestamped line; otherwise, indented lines join the line above. |
| Timestamps | ISO 8601 / RFC 3339, `2026/10/11 10:15:02` (nginx, Go), syslog `Oct 11 10:15:02` (the year is inferred), Apache `[Mon Oct 11 …]`, access-log `[11/Oct/2026:10:15:02 +0000]`, klog, and RFC 5424, plus JSON/CSV timestamp fields and epoch numbers. Pick a time zone for timestamps that don't carry one. A record without a timestamp inherits the previous record's (marked "≈" in the UI). |
| Encodings | UTF-8 (with or without BOM), UTF-16 from Windows/PowerShell, gzip. The NUL padding left by logrotate's `copytruncate` is skipped. Archives (zip, tar, xz, …) and binary files are refused with a clear message. |

Choose **Parse as** a source to apply that source's parsing settings and tags. This also makes fingerprints match the source's live events.

**Limits.** Uploads are capped at `LAYA_MAX_IMPORT_BYTES` (100 MB by default; gzip shrinks logs 5–30×), and decompressed content at 512 MB per file. One file is analyzed at a time. Analysis pauses whenever live ingest is backed up, so it never crowds out live logs. A file that hits a limit, the 45-minute time limit, or malformed data part-way through still gets a report covering everything read before the stop. The report says it is partial and why.

**Memory.** Every event is counted in the report, but only a sample is kept to browse: up to 5,000 events or 16 MB per file, preferring the most severe events plus a few examples of every pattern. The last 5 reports are kept in memory, and browsable events across them share `LAYA_IMPORT_MEMORY_MB` (64 MB), oldest freed first. Reports are cleared when Laya restarts. Download anything you want to keep.

Throughput with the default rules is roughly 20–30k lines per second on one core (measured on a mixed Java application log); `laya analyze` uses all cores.

**From a terminal.** The same analysis runs offline with no size limits, using all CPU cores:

```sh
laya analyze app.log                         # summary: severities, categories, top issues, routes
laya analyze -tz Europe/Berlin -source api app.log.gz app.log.1
laya analyze -output json app.log | jq .     # one full report per file
laya analyze -output ndjson -min-severity error app.log > errors.ndjson
laya analyze -fail-on critical deploy.log    # exit status 2 if anything critical (CI)
docker run --rm -i ghcr.io/vipinmohan22/laya analyze - < app.log
```

Rules come from `-config`, else `$LAYA_CONFIG`, else `$LAYA_DATA_DIR/config.json`, else the built-in defaults. A config exported from **Settings → Backup** works, so you can analyze locally with your team's rules. `laya analyze` uses only rules, sources and route filters. It never builds notifiers, and it makes no network calls unless you give it `-engine`.

With `-engine`, the Laya engine classifies every pattern the rules leave, with no per-file limit, using the config's categories and engine settings (`min_severity`, `min_confidence` and so on). Only the text of those patterns is sent to the engine. Before reading any file it checks that every engine URL answers and has the checkpoints it needs loaded, and exits with an error otherwise:

```sh
laya analyze -config config.json -engine http://127.0.0.1:8000 -engine-key "$LAYA_ENGINE_API_KEY" app.log
laya analyze -engine http://10.0.0.5:8000,http://10.0.0.6:8000 -engine-model multilingual app.log
```

`-engine` takes one or more comma-separated URLs, `-engine-model` overrides the config's checkpoint, and `-engine-key` defaults to `$LAYA_ENGINE_API_KEY`. The engine doesn't see source or file names. `-source` decides the parse settings, tags and checkpoint, and which source's cached answers the file can reuse.

**Over HTTP:**

```sh
curl -H "Authorization: Bearer $LAYA_ADMIN_TOKEN" -F file=@app.log https://laya.example.com/api/imports
curl -H "Authorization: Bearer $LAYA_ADMIN_TOKEN" --data-binary @app.log.gz \
  "https://laya.example.com/api/imports?name=app.log.gz&source=<source id>&tz=UTC&format=auto&engine=1"
```

The response is the finished report. `GET /api/imports/{id}/events` (and `?format=ndjson`), `/patterns` and `/events/{eventId}` return the details, and `/training-data` exports the file's rule-labelled patterns for [fine-tuning](#training-data-and-fine-tuning).

### Classification

1. **Severity detection.** Laya reads explicit level fields (names such as `level`, `severity`, `@l`, `severityText`; syslog 0–7; pino/bunyan 10–60) and the levels formats write into the text: a syslog `<PRI>`, klog prefixes (`E1011 …`), `level=`/`"level":` keys, and level words near the start of the record (within 96 characters after any syslog header) that are uppercase (`ERROR …`, `WARN[0042]`, `[WRN]`), bracketed (`[crit]`, `<warn>`, `[core:error]`), a label (`app[42]: warning: …`, `main.go:42: error:`, `PHP Warning:`, `fail:`), or open the line with a colon, tab or column spacing after them (`error: …`, `2026-10-11T10:00:00Z info   …`); plus fixed-place level columns of common daemons (MongoDB, Redis, etcd, PostgreSQL csvlog, java.util.logging, Monolog, NestJS, dmesg, kernel `BTRFS error`, npm, …). The record's first level wins. Lowercase level words inside a sentence (`user info`, `debug symbols`, `error rate`, `2026/10/11 10:00:00 error connecting …` from Go's level-less `log`) are not levels, and neither are words in request paths, echoed commands, JSON values or quoted settings (`GET /logs?level=error`, `COMMAND=… FATAL`, `set to "WARN"`). A level field of 3 or 6 (journald's default for a service's stdout and stderr, Docker's stream levels) or a syslog PRI with those severities gives way to a level the text states. A severity found this way is *stated* by the record. Otherwise Laya *guesses* it from keywords such as panic/fatal/outage/breach, error/exception/failed/failing/refused/unable to/cannot/timed out (and a message ending in "permission denied" or "no such file or directory"), and warn/deprecated/retrying, ignoring URLs, paths, mail addresses, negations like "no errors" and zero counters (`failed=0`). It defaults to *info* and marks the event `severity_guessed`. A rule that sets a severity makes it stated. `internal/classify/testdata/severity/` holds the corpus of real formats this is tested against.
2. **Rules, top to bottom.** Conditions test `message`, `level`, `severity`, `source`, `category`, `tags`, or any structured field by dot path (`http.status_code`). The operators are contains, equals, starts/ends with, regex (RE2, so no catastrophic backtracking), exists/missing, and numeric `>`/`<`, each with a negated form where it makes sense. Every matching rule adds tags and can set the severity or raise it to a minimum. The first matching rule with a category decides the category. **Drop** discards the event and **Stop** ends evaluation. Rules can be limited to specific sources.
3. **Fingerprint.** The message is reduced to a pattern (numbers, IPs, UUIDs, hex ids and timestamps replaced by placeholders) and hashed with the source. The fingerprint groups the dashboard's "Top issues" and drives notification throttling. The Laya engine's answers are cached by the text the engine reads instead (see below).
4. **Laya engine.** What the rules leave uncategorized, at *warning* or above, goes to the [Laya engine](#laya-engine), which may fill in the category, and the severity when it was only guessed.
5. **Learning.** A confident engine answer becomes a [learned rule](#learned-rules), so the next event of that pattern is classified at step 2.

Conditions can also test `pattern`: the message lowercased, with numbers, IPs, UUIDs, hex ids and timestamps replaced by `<n>`, `<ip>`, `<uuid>`, `<hex>` and `<ts>` (the first 400 bytes). `pattern equals "shipment <n> stuck in queue"` matches every shipment number. A rule can also keep matching events away from the engine (**Don't ask the Laya engine**, `skip_engine`).

Laya ships with editable default rules for crashes and panics, resource exhaustion, security attacks, auth failures, HTTP 5xx, database, network, slow operations, exceptions and deployments. The **Rule tester** runs a pasted sample through the rules without storing it, and the rule editor can test a draft rule on its own.

### Learned rules

Rules are the main classifier, and the Laya engine handles what they miss. Learning turns the engine's answers into rules, so each new kind of problem costs one engine call:

1. A live event that no rule categorizes goes to the engine, which answers with one of your categories.
2. When that answer is applied, at least `learn_min_confidence` confident (0.8 by default, and never below `min_confidence`), Laya adds a rule at the end of the list. The rule tests `pattern equals "<the event's pattern>"`, applies only to the event's source, and sets the category. It also sets the severity (as a minimum) when the engine's severity answer was applied. Proposals are written in batches, one config write per second at most.
3. The rule is active at once, so the event and the ones after it are classified and alerted on as the category says. It is marked **under review** and appears under **Classification rules → Under review**, with the engine's confidence, the checkpoint and the message it was learned from. The sidebar shows how many await review, and so does the dashboard.
4. Review each one:
   - **Approve** it, and it becomes an ordinary rule (labelled *learned*).
   - **Edit** it, for example to correct the category or widen the condition, then **Save & approve**.
   - **Reject** it. A rejected rule loses its category and severity but stays, enabled, with **Don't ask the Laya engine** set, so the pattern stays uncategorized and the model isn't asked about it, or learned from, again. **Restore** applies what was learned again.
   - **Delete** it, and the next event of the pattern goes to the engine again, which may teach the same rule anew.

Only live events teach rules; file analysis never does. At most 500 learned rules may await review: past that, Laya learns nothing new (and the dashboard says so) until some are reviewed. Turn learning off, or change the confidence it needs, under **Settings → Laya engine → Learning** (`learn_rules`, `learn_min_confidence`). Over the API, `POST /api/rules/review` with `{"action": "approve"|"reject", "ids": [...]}` reviews one or many. Learned rules carry where they came from in `learned` (`at`, `model`, `category`, `confidence`, `source_id`, `event_id`, `example`) and their state in `status` (`review`, `rejected`, or empty once approved).

A learned rule is only as good as the answer it came from, and zero-shot the published checkpoints are often wrong (see **Accuracy** below). Rules under review do alert, so review them, especially before routing pages on their categories. A higher `learn_min_confidence` learns less, and less often wrongly.

### Alert groups

An **alert group** is a team, or a set of people, alerted about specific sources:

- **Sources.** The sources whose events it hears about. Assign them in the group, or under **Alerts** in each source's settings. A source can have several groups.
- **Which events.** A minimum severity (*error* by default) and, optionally, categories.
- **Channels.** Notifiers the group is reached on: its Slack or Teams channel, PagerDuty service, a webhook.
- **Members.** Email addresses, emailed through an email notifier you pick. Its SMTP server and From address are used, with the members as the recipients instead of the notifier's own.
- **Throttling**, as for routes.

Groups work alongside alert routes: an event goes to every route and every group of its source that it matches. A group with no sources gets nothing. Deliveries show the group as the route (`Alert group: <name>`), and file analysis reports list the groups that would have been alerted. The API is `/api/alert-groups` (`GET`, `POST`, `PUT /{id}`, `DELETE /{id}`), where `source_ids` sets the group's sources; sources list their groups in `alert_group_ids`.

### Notifiers and routes

A **route** selects events by minimum severity, categories, sources, tags and an optional message regex, and sends them to one or more **notifiers**. With throttling on, after alerting on a problem the route suppresses the same fingerprint on that notifier for the window you choose. The next alert says how many similar events were suppressed. Each notifier has its own queue (10,000 notifications, as many as the pipeline holds), so a slow or failing one delays only its own alerts; if a queue fills anyway, alerts are dropped, counted (`dropped` on the dashboard and in `laya_notifications_total`) and logged. Deliveries are retried on network errors, 429s and 5xx responses (3 attempts with backoff, none while shutting down), and every attempt appears under **Deliveries**.

| Notifier | Notes |
| --- | --- |
| Email (SMTP) | STARTTLS, implicit TLS or plaintext. HTML and plain-text bodies. Multiple recipients. |
| Slack | Incoming webhook with colour-coded blocks and an optional mention on critical events. Also works with Mattermost and Rocket.Chat. |
| Microsoft Teams | Adaptive Card. Works with Teams Workflows ("Post to a channel when a webhook request is received") and legacy incoming webhooks. |
| Discord | Embed, with mentions disabled so log text can't ping `@everyone`. |
| Google Chat | Space webhook. |
| PagerDuty | Events API v2. The fingerprint is the dedup key, so repeats update one incident. |
| Generic webhook | Any method, URL and headers. The body is a Go `text/template` (`{{ json .Title }}`, `.Event.Severity`, `.Event.Fields`, `.Link`, …). Leave it empty for Laya's standard JSON payload. Use it for Opsgenie, ntfy, Zulip, internal services and so on. |

**Secrets.** Any notifier setting, pull-source credentials, headers and POST body, and the Laya engine's URL and key can refer to a value kept outside Laya's config file, so secrets can live in Nomad Variables, Vault or a secrets file:

- `${env:NAME}` is the environment variable `NAME`. When `NAME` is unset and `NAME_FILE` is set, it is the contents of the file `NAME_FILE` names (the `_FILE` convention of Docker and Kubernetes secrets).
- `${file:/path}` is the contents of that file. Only files under Nomad's secrets and task directories (`$NOMAD_SECRETS_DIR`, `$NOMAD_TASK_DIR`), `/run/secrets` and the directories listed in `LAYA_FILE_REF_DIRS` can be referenced, by an absolute path without `..` (symlinks are followed, and must stay inside), so the API can't be used to read other files on the host. Adding a reference to any other file is refused. A config saved earlier with such a reference still loads: Laya logs a warning at startup and the reference reads as empty. `laya analyze` reads any file its user can.

File contents lose their trailing newline, and a file that can't be read, isn't a regular file or is over 64 KiB gives an empty value. Notifiers read their references when the configuration is applied (at start and whenever it is saved), pull sources on every poll, and the engine URL and key every 30 seconds, so a file can change while Laya runs. Secret fields are masked in the API and UI (references themselves are shown), and saving a form with the masked value unchanged keeps the stored secret.

**Adding a notifier type.** Create a file in `internal/notify/` that calls `Register(&Plugin{...})` from `init()`, declaring its fields (label, type, required, secret, help) and a constructor that returns a `Sender`. The UI renders the form from that schema and nothing else needs to change. See `chat.go` for compact examples.

### Laya engine

The Laya engine is [`laya-serve`](https://github.com/NandhaKishorM/laya), a small Python/PyTorch decision model served over HTTP. It runs locally, next to Laya (`make up` starts it with Laya, and the Nomad deployment includes a job for it), so log text never leaves your infrastructure. Laya itself stays a single Go binary: when no engine is configured or the engine is down, it classifies with its rules alone. Laya uses it for what the rules can't place:

1. **Rules first.** Events go through severity detection and the rules as usual.
2. **What goes to the engine.** An event is sent when its severity, stated or guessed, is at least `min_severity` (*warning*) and, with `only_uncategorized` on (the default), no rule gave it a category. A message with no level and no telling keyword is guessed *info*, so at the default it isn't sent.
3. **The questions.** Laya asks which of your categories the event belongs to: a choice over your categories in the order you list them, offered by name (with the description, where you gave one), plus `uncategorized` offered as "none of the above". With `set_severity` on (off by default: it about doubles the engine's time per event, and in testing rarely changed a keyword guess), when the severity was only guessed (`severity_guessed`, see [Classification](#classification)), it also asks how severe the event is, on the scale from *debug* to *critical*. A severity the record states or a rule sets is never questioned.
4. **Confidence.** Each answer, category and severity, is used only when its confidence is at least `min_confidence` (0.6). Below that it's recorded on the event and nothing changes.
5. **Applied.** An answer counts as *applied* only when it changed the event: an uncategorized event got one of your categories, or a guessed severity became another one. An answer of `uncategorized`, a severity equal to the guessed one, or an answer below the threshold changes nothing.
6. **Rules win.** The engine only fills in a missing category and corrects a guessed severity. It never overrides a rule.
7. **Learned.** An applied answer at least `learn_min_confidence` confident becomes a [learned rule](#learned-rules) for the pattern, under review, so the engine isn't asked about it again.
8. **Once per pattern.** Answers are cached per source by everything the engine reads about an event (the message, level and fields, with numbers, IPs, UUIDs, hex ids and timestamps normalized as in patterns) and the checkpoint (and whether the severity was asked) for 24 hours, up to 5,000 answers, and simultaneous events that read the same share one request, so a recurring error costs one call. Fields that identify a request, trace, session or user (`request_id`, `traceId`, `user`, `email`, any `*_id`…) aren't sent, so they don't make each event look new; other fields that differ per event (free text, paths with names) do cost a request each. Answers aren't shared between sources: two texts that normalize the same can still read differently to the engine (numbers, case), and one source's events must not decide another's. File analysis read as a source (**Parse as**) reuses that source's answers and adds to them; files read without a source share answers only with each other. Changing the categories, a checkpoint (the default or a source's) or the key clears it, and so does a new checkpoint revision reported by the engine; instances coming and going (Nomad scaling or rescheduling) don't. A new `min_confidence` applies to cached answers at once.

Each event that was sent carries the engine's answer in `engine`: the checkpoint (`model`), the chosen `category` and its `confidence` (no category when only the severity was asked), the `top` three categories, the `severity` and `severity_confidence` when it was asked, the `threshold` (`min_confidence` when the answer was used), whether it was `applied` (and whether to the `category_applied` or `severity_applied`), and whether the event was `truncated` to fit the model. Its `classifier` is `laya` when the engine's category was applied (a severity alone doesn't change it).

**Load and waiting.** laya-serve computes one answer at a time; further requests wait inside it, where Laya can neither see them nor give up on them early. So `max_in_flight` is counted per engine instance and defaults to 1, and events wait in Laya's own queue (256 events) instead. An event waits at most `timeout_seconds` (30) for its answer, queueing included, so the engine delays an alert by at most that. From recent request times and the capacity (`max_in_flight` × the instances Laya can ask: those that passed their last health check and accept its key), Laya estimates how long a new event would wait: one that would run out of time in the queue isn't queued at all, and goes on at once with the rules' result (it is *shed*). More instances add capacity: requests go round-robin to the instances that passed their last health check, with failover between them.

**Failures.** Whenever the engine can't answer in time, the event goes on with the rules' result. Then:

| What happened | What Laya does |
| --- | --- |
| The engine is busy (HTTP 503) | Retries after its `Retry-After` until the event's time is up. A busy engine is never paused. |
| A request runs out of time | Moves on. Three in a row pause calls, with backoff from 5 s up to 2 minutes. |
| An instance is unreachable or fails (5xx) | Tries the next instance, then pauses calls with the same backoff. Errors from requests already in flight count once. |
| The key is rejected (401/403) or the endpoint is missing (404) | Pauses all calls for 5 minutes. |
| The answer came from another checkpoint | laya-serve answers a checkpoint name it doesn't know with another checkpoint, without saying so. Laya checks the name in every answer and pauses that checkpoint alone for 5 minutes; other checkpoints carry on. |
| The answer is malformed: a category that wasn't offered, or a confidence that isn't a probability | Treated as a failing instance. Only the probabilities of the offered categories are kept. |
| The checkpoint isn't loaded on the engine (as it reported at the last health check) | Laya doesn't ask for it. laya-serve would load it on the first request: a second model in memory (about 2 GB more), which can exceed the instance's memory limit, evict another checkpoint (`LAYA_MAX_LOADED`) or start a download. Events that need it keep the rules' result, and the engine status warns. So load every checkpoint you use at startup (`LAYA_MODELS`), and don't let laya-serve unload them (`LAYA_IDLE_UNLOAD_SECONDS`): Laya won't ask for one again until the engine reports it loaded. Checkpoints in `LAYA_MODELS` stay loaded whatever `LAYA_MAX_LOADED` says. |

Laya checks every instance's `/health` every 30 seconds: which instances answer and which checkpoints they have loaded. A passing check lifts a pause caused by timeouts or connection failures. Changing the URL, key, checkpoints or categories lifts every pause at once.

**Categories.** Under **Settings → Categories**, list your categories in the order you want them offered. Plain names work best, and the defaults are just names: `security`, `auth`, `application`, `database`, `network`, `resource`, `http`, `performance`, `deployment`, `audit`. Add a short description only to tell apart names the engine confuses. With ten or more categories the engine reads only about 16 tokens of each option, and Laya's engine status warns when the engine couldn't tell categories apart. On 23 hand-labelled log lines with the english checkpoint, plain names plus "none of the above" picked the right category 14 times, against 9 with a few keywords as each category's description (the earlier defaults), and answered faster (about 0.8 s against 1.4 s per event on 2 CPU threads). That is a small sample: read it as a hint, not a benchmark, and check your own logs with the tester below.

Names are lower-cased and limited to 64 characters, descriptions to 300, and there can be up to 50 categories. A description needs a name, and `uncategorized` is reserved for events no category fits, so it is refused as a name. A name listed twice is merged, unless the two descriptions differ, which is an error. Blank rows are dropped. Configs that list categories as plain strings still load.

**Checkpoints.** `model` selects the checkpoint laya-serve answers with, and Laya names it in every request:

| Checkpoint | |
| --- | --- |
| `english` | The default. English text; reads about the first 460 tokens of an event. |
| `multilingual` | Many languages, English included; reads about 980 tokens. |
| `typed-decisions` | Fine-tuned on upstream's typed-decisions benchmark, not on logs. |
| your name | A fine-tuned checkpoint registered on the engine with `LAYA_EXTRA_MODELS` (see [fine-tuning](#training-data-and-fine-tuning)). |

Aliases such as `en`, `multi` and `typed` are accepted. A source can override the checkpoint (**Laya engine checkpoint** in the source editor, `engine_model` in the API), for example `multilingual` for a source whose logs aren't in English. The engine must load every checkpoint you use at startup (`LAYA_MODELS`); Laya doesn't send events to a checkpoint the engine hasn't loaded (see **Failures**).

Laya sends each event as compact text: the first line, the root cause of a stack trace (`Caused by:`, the exception line, `panic:`), level, source and structured fields, then the rest of the message. That way the model's limited input holds the telling parts.

**Settings.** The engine settings are under **Settings → Laya engine** in the UI, and `settings.engine` in `GET`/`PUT /api/settings` and the config file. A `PUT` changes only the settings it includes; settings a config file or an imported export doesn't mention (one written before the engine existed, say) get their defaults:

| Setting | Default | |
| --- | --- | --- |
| `enabled` | `true` | Idle until a URL is set. |
| `url` | `${env:LAYA_ENGINE_URL}` | laya-serve base URL. Several URLs, comma-separated, are used round-robin with failover. [References](#notifiers-and-routes) (`${env:…}`, `${file:…}`) are re-read every 30 seconds. Credentials in a URL are masked in the API (`http://user:xxxxx@host`, or `http://xxxxx@host` for a user alone) and errors; an entry sent back masked keeps the credentials of the same address (or the same host, or the only credentials stored), so instances can be added or moved without typing them again. Laya connects directly: `HTTP_PROXY`/`HTTPS_PROXY` don't apply to the engine. |
| `api_key` | `${env:LAYA_ENGINE_API_KEY}` | The engine's `LAYA_API_KEY`. Masked like other secrets. |
| `model` | `english` | The checkpoint (see above). Names are lower-cased, as laya-serve's are. |
| `only_uncategorized` | `true` | `false` also asks about events a rule categorized. Rules still win, so that only records the engine's opinion on the event, at the cost of more calls. |
| `min_severity` | `warning` | Compared with the stated or guessed severity. |
| `min_confidence` | `0.6` | 0 to 1, for the category and the severity answer alike. |
| `set_severity` | `false` | Ask for a severity when it was only guessed (step 3). About doubles the engine's time per event. |
| `max_in_flight` | `1` | Concurrent requests per engine instance, at most 16. See **Load and waiting**. |
| `timeout_seconds` | `30` | The longest an event waits for an answer, queueing included, at most 300. |
| `learn_rules` | `true` | Turn applied category answers into [learned rules](#learned-rules) awaiting review. Engine settings saved before learning existed get it on. |
| `learn_min_confidence` | `0.8` | The confidence an answer needs to become a rule (never less than `min_confidence`). |

Larger values are refused when you save.

**Accuracy: set your expectations.** The published checkpoints are general decision models that were not trained on logs, and zero-shot they are weak at log classification: they often answer `uncategorized`, or pick a plausible but wrong category, and their confidence is not calibrated. Upstream says the same of its own benchmark: the base checkpoints score near chance zero-shot (0.36 against a 0.32 random baseline) and 0.77 after fine-tuning, and it recommends fine-tuning for accuracy. So keep the rules as your main classifier, keep the confidence threshold, check what the engine says about your own logs with `POST /api/engine/classify` before you route alerts on its categories, and [fine-tune](#training-data-and-fine-tuning) for your categories if you rely on it.

**Trying it.** **Settings → Laya engine** has **Check connection** and a **Classify** box for a log line. Over the API, `POST /api/engine/classify` asks the engine about a pasted message or a stored event right now. It has no side effects in Laya: it neither reads nor fills the cache, isn't counted in the engine statistics, ignores pauses and never causes one, and changes no event. It also asks about events the live pipeline wouldn't send (below `min_severity`, or categorized by a rule). It refuses a checkpoint the engine hasn't loaded, as live classification does (asking would make laya-serve load it). The reply includes the rules' result for comparison:

```sh
curl -H "Authorization: Bearer $LAYA_ADMIN_TOKEN" -X POST http://localhost:8080/api/engine/classify \
  -d '{"message": "could not obtain JDBC connection from pool after 30000ms, request failed"}'
```

```json
{"ok": true,
 "result": {"model": "english", "category": "database", "category_asked": true, "category_confidence": 0.9979,
            "probabilities": {"database": 0.9979, "application": 0.0018, "performance": 0.0002, "…": 0},
            "severity": "error", "severity_confidence": 0.5911, "latency_ms": 1338, "inference_ms": 1335},
 "rules": {"category": "uncategorized", "severity": "error", "severity_guessed": true, "matched_rules": null, "dropped": false}}
```

The rules guessed *error* from "failed" (and `set_severity` was on in this example, so the severity was asked too; it is off by default). The category answer clears the threshold and would be applied; the severity answer is below it (and the same as the guess). Add `"source_id"` to classify a message as that source would (its checkpoint), or pass `{"event_id": "…"}` instead for a stored event. `dropped` says a rule would drop such an event, so live it would never reach the engine. For a stored event, `rules` is what the rules alone decide today (including `severity_guessed`), not the event's stored result, which may include the engine's answer. With no categories configured only the severity can be asked (`category_asked` is false); when it isn't asked either, the tester answers that there is nothing to ask. Errors come back as `{"ok": false, "error": "…"}`.

**Health and status.** `GET /api/engine/health` checks every configured instance now. `ok` is true when each one answers, lists its checkpoints (laya-serve shows them only to callers with its key, so a wrong or missing key makes `ok` false, with an error saying so) and has the default checkpoint loaded. Each endpoint reports `url`, `ok`, `error`, `loaded`, `missing` (checkpoints in use, a source's included, that it hasn't loaded), `model_loaded` (the default checkpoint is loaded), `device`, `revision` and `latency_ms`. Until the engine has reported its checkpoints, Laya asks it only with the default checkpoint, and checks its health every 5 s instead of every 30 s. The `engine` section of `GET /api/stats`:

| Field | |
| --- | --- |
| `enabled`, `configured`, `config_error` | Whether the engine is on, whether a usable URL is set (after expanding references), and why not. |
| `urls` | The instances in use, credentials hidden. |
| `model`, `models_in_use` | The default checkpoint, and every checkpoint in use: the default and the sources' own. |
| `loaded` | The checkpoints the engine reported at its last health check, across instances. |
| `capacity`, `in_flight` | `max_in_flight` × the instances Laya can ask (up, and accepting its key), and the requests open now. |
| `calls_total` | Requests made, for live events and file analysis. |
| `cache_hits` | Live events answered from the cache or by another event's request. |
| `applied`, `low_confidence` | Live answers that changed the event; live category answers below `min_confidence`. |
| `errors`, `timed_out` | Failed requests; events whose request ran out of time. |
| `skipped` | Eligible live events not sent: no free slot in time, the queue full or the wait too long (shed), calls paused, or the checkpoint not loaded. |
| `truncated` | Events cut to fit the model. |
| `avg_latency_ms`, `avg_inference_ms` | Recent round trips (including time queued inside the engine), and the engine's own compute time. |
| `cached_results` | Patterns in the cache. |
| `last_error`, `last_error_at`, `last_success_at`, `paused_until` | The last failure, the last answer (after it, the error is history), and until when all calls are paused. |
| `paused_checkpoints` | Checkpoints paused alone because the engine answered with another. |
| `warnings` | Setup problems: checkpoints in use but not loaded, checkpoints paused because the engine answered with another, categories the engine can't tell apart. |

The `pipeline` section of `/api/stats` adds `engine_queued`, `engine_queue_depth` and `engine_shed` (events not queued because they would have run out of time; they are also in the engine's `skipped`).

**Running the engine for development.** laya-serve needs Python 3.10 or newer. Install the CPU build of PyTorch first unless you have a GPU:

```sh
python3 -m venv ~/laya-engine && . ~/laya-engine/bin/activate
pip install torch==2.14.0 --index-url https://download.pytorch.org/whl/cpu
pip install "laya[serve]==0.4.2"

LAYA_HOST=127.0.0.1 LAYA_PORT=8000 LAYA_MODELS=english LAYA_DEVICE=cpu LAYA_THREADS=4 \
  TORCH_DISABLE_NATIVE_JIT=1 TOKENIZERS_PARALLELISM=false LAYA_API_KEY=dev-engine-key \
  laya-serve
```

The first start downloads the english checkpoint (about 0.85 GB) into `~/.cache/huggingface` and loads it, which takes about half a minute; later starts take 10–25 s, and `HF_HUB_OFFLINE=1` then keeps it off the network. It needs about 2.5 GB of memory (3 GB while loading) and answers a question in about half a second on 4 cores. Never set `LAYA_DEVICE=auto`, which crashes it; use `cpu` or `cuda`. Then start Laya pointed at it:

```sh
LAYA_ENGINE_URL=http://127.0.0.1:8000 LAYA_ENGINE_API_KEY=dev-engine-key \
  LAYA_ADMIN_TOKEN=change-me-to-something-long make run
```

#### Training data and fine-tuning

Fine-tuning is where the engine's accuracy comes from, and your rules already label many events. Two endpoints export them in `laya-train`'s JSONL format:

- `GET /api/engine/training-data`: from the live events in memory;
- `GET /api/imports/{id}/training-data`: from the events kept for browsing an analyzed file, which include examples of every pattern.

Each line is one text the engine would read (numbers, ids and times aside, from any source) that a rule gave one of your categories; events whose category the engine set are left out, and so are file events cut to keep them for browsing (over 8 KB, or very large fields), whose text would no longer match their label. `state` is exactly the text Laya sends the engine for that event, `questions` is the category question Laya asks (your current categories in order, plus `uncategorized`), and `expected` is the rule's category:

```json
{"state": "java.lang.NullPointerException at com.example.Foo.bar(Foo.java:42)\nlevel: error\nservice=api",
 "questions": {"category": {"type": "choice", "instructions": "Which category does this log event belong to?",
               "criteria": {"security": null, "auth": null, "application": null, "…": null, "uncategorized": "none of the above"}}},
 "expected": {"category": "application"}}
```

`laya-train` comes with laya-serve's package; upstream's [Fine-tuning Laya on your own decisions](https://github.com/NandhaKishorM/laya/blob/v0.4.2/docs/finetune.md) describes it. It runs on CUDA, MPS or CPU. A full fine-tune is slow on a CPU; `--freeze-encoder` trains only the decision head, which fits a CPU but gains much less.

```sh
curl -H "Authorization: Bearer $LAYA_ADMIN_TOKEN" -o train.jsonl http://localhost:8080/api/engine/training-data
laya-train --data train.jsonl --base english --dry-run     # counts usable items; loads no weights
laya-train --data train.jsonl --base english --out ./laya-logs --eval held-out.jsonl
```

Then copy `./laya-logs` to the engine, register it with `LAYA_EXTRA_MODELS='{"logs": "/path/to/laya-logs"}'`, add `logs` to `LAYA_MODELS` (on Nomad: `extra_models` and `models`), and select `logs` as the checkpoint in **Settings → Laya engine** or on a source.

- The labels are your rules' decisions, mistakes included. What you gain is a model that also recognizes similar messages the rules don't match.
- The export has no `uncategorized` examples. To keep the model answering "none of the above" for messages that fit no category, add lines of the same shape with `"expected": {"category": "uncategorized"}`.
- The question is the one for your current categories. After changing them, export and train again, since Laya then asks a different question.
- One line per pattern makes for small files: combine exports from several days and files. With fewer than about 100 lines, laya-train can't fit calibration (it warns `not fitted`) and the checkpoint's confidences are unreliable for `min_confidence`. Keep some lines out of training and pass them with `--eval` to compare the base checkpoint with yours.

## Configuration

Runtime configuration (sources, rules, notifiers, routes, settings) is edited in the UI and stored in `$LAYA_DATA_DIR/config.json`, which is written atomically with mode 0600. **Settings → Backup** exports and imports it as JSON. Exports carry neither the admin token nor the setup wizard's progress, and an import keeps this instance's own.

| Environment variable | Default | Purpose |
| --- | --- | --- |
| `LAYA_ADMIN_TOKEN` | the token set in Laya (`admin-token.json`), else random per run (logged) | Bearer token for the UI and API. Set it to a long random value. When set, it is the only admin token; see [Admin token](#admin-token). |
| `LAYA_LISTEN` | `:8080` | HTTP listen address. |
| `LAYA_DATA_DIR` | `./data` | Holds `config.json` and `admin-token.json`. |
| `LAYA_PUBLIC_URL` | derived from requests | External URL for ingest instructions and notification links (the UI setting takes precedence). |
| `LAYA_CONFIG_SEED` | — | JSON config file used on first start only, when `config.json` doesn't exist yet. Add `"onboarding": {}` to it to offer the [setup wizard](#first-run). |
| `LAYA_EVENT_BUFFER` | `5000` | Recent events kept in memory for the UI. |
| `LAYA_QUEUE_SIZE` | `10000` | Pipeline queue. When it's full, ingest returns 503 with `Retry-After`. |
| `LAYA_WORKERS` / `LAYA_ENGINE_WORKERS` / `LAYA_NOTIFY_WORKERS` | `4` / `16` / `4` | Concurrency. Engine workers hold events waiting for the Laya engine; requests to it are capped by `max_in_flight` per engine instance. |
| `LAYA_ENGINE_URL` | — | Laya engine (laya-serve) base URL, or several comma-separated. Read by the default engine settings (`${env:LAYA_ENGINE_URL}`); unset (and no file below) means rules only. |
| `LAYA_ENGINE_URL_FILE` | — | Used when `LAYA_ENGINE_URL` is unset: a file holding the URL(s), re-read every 30 seconds. The Nomad job renders engine addresses from service discovery into one. |
| `LAYA_ENGINE_API_KEY` / `LAYA_ENGINE_API_KEY_FILE` | — | The engine's bearer key (its `LAYA_API_KEY`), or a file holding it, read by the default engine settings. |
| `LAYA_FILE_REF_DIRS` | — | More directories (`:`-separated) whose files `${file:…}` references may read, besides `$NOMAD_SECRETS_DIR`, `$NOMAD_TASK_DIR` and `/run/secrets`. |
| `LAYA_MAX_INGEST_BYTES` | `10485760` | Maximum request body (compressed). Decompressed bodies may be up to 4× this. |
| `LAYA_MAX_IMPORT_BYTES` | `104857600` | Largest log file upload on the Log files page (as sent, so possibly gzipped). |
| `LAYA_IMPORT_MEMORY_MB` | `64` | Memory for browsable events across kept log file analyses. |
| `LAYA_LOG_LEVEL` / `LAYA_LOG_FORMAT` | `info` / `json` | Laya's own logs (`text` for humans). |

Endpoints:

- `GET /healthz` returns liveness (used by the Nomad check).
- `GET /metrics` serves Prometheus metrics: events received, processed, dropped and rejected; queue depths; last-hour events by severity; notification outcomes; log file analyses; pull-source health; and the Laya engine (below).
- `POST /ingest/{source}` accepts logs.
- `/api/*` is the REST API the UI uses. Everything requires the [admin token](#admin-token) (`Authorization: Bearer …`). Besides the configuration (`/api/sources`, `/api/rules` and `/api/rules/review`, `/api/notifiers`, `/api/routes`, `/api/alert-groups`, `/api/settings`, `/api/config/export` and `/import`):
  - `GET /api/stats` (counters and engine status) and `GET /api/events`;
  - `GET /api/auth` says where the admin token comes from, `PUT /api/auth/token` sets a new one (`{"token": "…"}`, sent with the current one) and `DELETE /api/auth/token` deletes a stored token that `LAYA_ADMIN_TOKEN` overrides;
  - `GET /api/onboarding` is the setup wizard's progress, and `POST /api/onboarding/start`, `/steps/{step}`, `/defaults`, `/defer`, `/complete` and `/dismiss` change it;
  - `POST /api/imports` analyzes a log file (`?engine=1` to ask the Laya engine), and `GET /api/imports/{id}` (with `/events`, `/patterns` and `/training-data`) returns the results;
  - `GET /api/engine/health` checks the Laya engine, `POST /api/engine/classify` asks it about one message or event, and `GET /api/engine/training-data` exports laya-train data (see [Laya engine](#laya-engine)).

The Laya engine's metrics:

| Metric | |
| --- | --- |
| `laya_engine_calls_total` | Requests to the engine, for live events and file analysis. |
| `laya_engine_errors_total` | Failed requests. |
| `laya_engine_timed_out_total` | Events whose request ran out of time. |
| `laya_engine_cache_hits_total` | Live events answered from the cache or by another event's request. |
| `laya_engine_applied_total` | Live answers that changed a category or severity. |
| `laya_engine_low_confidence_total` | Live category answers below `min_confidence`. |
| `laya_engine_skipped_total` | Eligible live events not sent: busy, shed, paused, or the checkpoint not loaded. |
| `laya_engine_queue_depth` | Events waiting for the engine. |
| `laya_engine_requests_completed_total` | Requests answered. |
| `laya_engine_request_seconds_total` | Time spent on answered requests, including time queued inside the engine. Divide its rate by the rate of `…_requests_completed_total` for the average. |
| `laya_engine_inference_seconds_total` | The engine's compute time for answered requests (its `X-Inference-Time-Ms`). |
| `laya_engine_paused` | 1 while all calls are paused after errors. |
| `laya_engine_checkpoints_paused` | Checkpoints paused alone because the engine answered with another. |
| `laya_rules_learned_total` | Rules learned from the engine's answers since Laya started. |
| `laya_rules_review` | Learned rules awaiting review. |

### Admin token

The UI and the API share one admin bearer token. Laya picks it at startup:

| `LAYA_ADMIN_TOKEN` | Token set in Laya | Admin token |
| --- | --- | --- |
| set | either | `LAYA_ADMIN_TOKEN`, and only it. A token set in Laya is ignored, with a warning at startup; delete it under **Settings → Access** so that removing the variable later can't bring it back. |
| unset | yes | The token set in Laya. Nothing is logged. |
| unset | no | A random token for this run, logged as `admin_token`. |

A token set in the UI (the setup wizard's Access step, or **Settings → Access**) is generated in the browser and shown, and is saved only once you confirm you have kept it. Laya stores it in `$LAYA_DATA_DIR/admin-token.json` (mode 0600) as an HMAC-SHA256 digest under a random key, never in plain text and never in `config.json`, so exports, imports and seeds don't carry it. A change applies at once: the tab that made it stays signed in, other tabs and API scripts need the new token, and the old one, a logged one included, stops working. If `admin-token.json` can't be read or parsed, Laya refuses to start rather than fall back to a logged token; fix or delete the file, or set `LAYA_ADMIN_TOKEN`. Releases before the setup wizard ignore the file, so after a downgrade sign in with `LAYA_ADMIN_TOKEN` or the logged token.

The token is an API key, not a password: use the generated one, a password manager's generator or `openssl rand -hex 24`, never a password you use elsewhere. A token set in Laya must be 32 to 256 printable ASCII characters without spaces, with at least 10 different characters and none making up more than a quarter of it. These rules catch mistakes; the strength comes from the token being random. `LAYA_ADMIN_TOKEN` isn't checked against them.

**Lost the admin token?** Set `LAYA_ADMIN_TOKEN` (on Nomad, `nomad var put nomad/jobs/laya LAYA_ADMIN_TOKEN=…`) and restart: it takes precedence. Or delete `admin-token.json` from the data directory and restart, and Laya logs a new token for the run.

## Operational notes

- **Single instance.** Configuration is one file on one volume, so the Nomad job runs `count = 1` and Nomad reschedules it if the node fails. Recent events and delivery history live in memory and reset on restart. Laya is a classifier and alerter, not long-term log storage; keep shipping logs to your log store as well (a webhook notifier can forward classified events).
- **Laya engine.** The engine is a separate service with its own needs: about 3.5 GB of memory and 4 CPU cores per instance on CPU, roughly 1–2 new message patterns per second each, or an NVIDIA GPU for far more. Laya never depends on it: without it, or while it's down, classification is the rules' alone. On Nomad, Laya re-reads the engine addresses from a file, so engine instances coming and going don't restart it; see [deploy/nomad/README.md](deploy/nomad/README.md#6-run-the-laya-engine-optional).
- **Shutdown.** On SIGTERM or Ctrl-C, Laya stops accepting requests and stops polling, finishes the events it already accepted (engine requests are cancelled, so those events keep the rules' result unless the engine already answered their pattern), then delivers the notifications still queued, without retrying failed ones. Requests in progress get 8 s (then they are cut off) and events 8 s; notifications get whatever is left of 26 s from the signal, and stopping 2 s more: 28 s at most, within Nomad's 30 s `kill_timeout`. Laya logs what was left if time runs out.
- **Security.** The UI sends a strict Content-Security-Policy and renders all log content as text, never HTML. Ingest tokens are compared in constant time. The admin token is checked through an HMAC-SHA256 digest, which is also all Laya stores of a token set in the UI; it is an API key, so the fast hash is enough (a generated one has 192 random bits). Pull sources and webhooks can reach any URL the admin enters, so treat the admin token like infrastructure credentials and put Laya behind TLS (for example Traefik or another load balancer).

## Development

```sh
make test            # go test -race ./...
make lint            # gofmt, go vet, JS syntax check
make build           # bin/laya
make up / make down  # Laya and the Laya engine with Docker Compose
make engine          # the Laya engine alone, for make run
make docker          # container image
make nomad-validate  # nomad fmt -check + job validate
```

The layout:

```
cmd/laya            entrypoint, env configuration, `laya analyze`
internal/config     config model, validation, JSON store, secret masking, setup defaults
internal/auth       admin token: precedence, storage as a keyed hash, rotation
internal/atomicfile atomic file replacement, for config.json and admin-token.json
internal/ingest     payload parsing (JSON / NDJSON / text / CSV), streaming file reader
internal/poller     pull-source polling
internal/classify   severity heuristics, rule engine, fingerprints
internal/engine     Laya engine client (laya-serve) and classifier
internal/learn      rules learned from the engine's answers
internal/pipeline   queue → classify → (Laya engine) → store → dispatch
internal/importer   log file analysis: reports, retention, timeline
internal/events     in-memory recent events and stats
internal/notify     notifier plugins, routing, throttling, retries
internal/server     REST API, ingest endpoint, metrics, UI hosting
web/static          the UI (plain JS, no build step)
deploy/nomad        Nomad jobs (Laya and the Laya engine), volumes and variables
```
