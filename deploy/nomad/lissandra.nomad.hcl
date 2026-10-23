# Lissandra: log ingestion, classification and alerting on Nomad.
#
#   nomad volume create deploy/nomad/lissandra-volume.hcl       # once
#   nomad var put nomad/jobs/lissandra LISSANDRA_ADMIN_TOKEN="$(openssl rand -hex 24)" \
#     LISSANDRA_ENGINE_API_KEY="<the engine's LAYA_API_KEY>"    # if you run laya-engine
#   nomad job run -var-file=deploy/nomad/lissandra.vars.hcl deploy/nomad/lissandra.nomad.hcl
#
# Classification by the Laya engine is optional: run laya-engine.nomad.hcl
# and this job finds it through service discovery. Without it, Lissandra
# classifies with its rules alone. See deploy/nomad/README.md for details.

variable "datacenters" {
  description = "Datacenters Lissandra may run in."
  type        = list(string)
  default     = ["*"]
}

variable "namespace" {
  description = "Nomad namespace for the job."
  type        = string
  default     = "default"
}

variable "image" {
  description = "Lissandra container image."
  type        = string
  default     = "ghcr.io/vipinmohan22/lissandra:latest"
}

variable "volume_name" {
  description = "Host volume holding Lissandra's config (dynamic volume from lissandra-volume.hcl, or a static host_volume in the client config)."
  type        = string
  default     = "lissandra-data"
}

variable "public_url" {
  description = "External base URL of Lissandra (e.g. https://lissandra.example.com). Used for ingest URLs in the UI and links in notifications."
  type        = string
  default     = ""
}

variable "service_provider" {
  description = "Service discovery provider: \"nomad\" (built in) or \"consul\"."
  type        = string
  default     = "nomad"
}

variable "service_tags" {
  description = "Extra service tags, e.g. Traefik or Fabio routing tags."
  type        = list(string)
  default     = []
}

variable "http_port" {
  description = "Static host port for the HTTP listener. 0 picks a dynamic port (use with a load balancer that reads service discovery)."
  type        = number
  default     = 0
}

variable "cpu" {
  description = "CPU in MHz."
  type        = number
  default     = 300
}

variable "memory" {
  description = "Memory in MB. Budget ~2 KB per buffered live event, plus import_memory_mb, plus ~100 MB headroom for an analysis in progress."
  type        = number
  default     = 384
}

variable "event_buffer" {
  description = "How many recent events Lissandra keeps in memory for the UI."
  type        = number
  default     = 5000
}

variable "max_import_mb" {
  description = "Largest log file (as uploaded, so possibly gzipped) the Log files page accepts, in MB."
  type        = number
  default     = 100
}

variable "import_memory_mb" {
  description = "Memory for browsable events from analyzed log files, across all kept analyses, in MB."
  type        = number
  default     = 64
}

variable "log_level" {
  description = "debug, info, warn or error."
  type        = string
  default     = "info"
}

variable "engine_service" {
  description = "Service name of the Laya engine (laya-engine.nomad.hcl), found through service_provider. Every registered instance is used. Empty turns discovery off."
  type        = string
  default     = "laya-engine"
}

variable "engine_url" {
  description = "Fixed engine URL(s), comma-separated, e.g. a load balancer in front of the engine. Overrides engine_service."
  type        = string
  default     = ""
}

locals {
  # The engine's address list for the task: every instance of the engine
  # service as http://address:port, comma-separated (Lissandra spreads
  # requests over them and fails over), or nothing when no instance is
  # registered, in which case Lissandra classifies with its rules alone.
  engine_lookup = var.service_provider == "consul" ? "service" : "nomadService"
  engine_urls = (
    var.engine_url != "" ? "${var.engine_url}\n" :
    var.engine_service == "" ? "\n" :
    <<-EOT
      {{ range $i, $s := ${local.engine_lookup} "${var.engine_service}" }}{{ if $i }},{{ end }}http://{{ if .Address | regexMatch ":" }}[{{ .Address }}]{{ else }}{{ .Address }}{{ end }}:{{ .Port }}{{ end }}
    EOT
  )
}

job "lissandra" {
  datacenters = var.datacenters
  namespace   = var.namespace
  type        = "service"

  group "lissandra" {
    # Lissandra keeps its configuration in a single file on the volume, so it
    # runs as exactly one instance. Nomad reschedules it if the node fails.
    count = 1

    network {
      port "http" {
        static = var.http_port
        to     = 8080
      }
    }

    volume "data" {
      type            = "host"
      source          = var.volume_name
      read_only       = false
      access_mode     = "single-node-single-writer"
      attachment_mode = "file-system"
    }

    service {
      name     = "lissandra"
      port     = "http"
      provider = var.service_provider
      tags     = concat(["lissandra", "logs", "metrics"], var.service_tags)

      check {
        name     = "lissandra-http"
        type     = "http"
        path     = "/healthz"
        interval = "10s"
        timeout  = "2s"
      }
    }

    restart {
      attempts = 5
      interval = "5m"
      delay    = "10s"
      mode     = "delay"
    }

    update {
      max_parallel      = 1
      health_check      = "checks"
      min_healthy_time  = "10s"
      healthy_deadline  = "3m"
      progress_deadline = "10m"
      auto_revert       = true
    }

    reschedule {
      delay          = "30s"
      delay_function = "exponential"
      max_delay      = "10m"
      unlimited      = true
    }

    task "lissandra" {
      driver = "docker"

      config {
        image = var.image
        ports = ["http"]
      }

      volume_mount {
        volume      = "data"
        destination = "/data"
      }

      env {
        LISSANDRA_LISTEN           = ":8080"
        LISSANDRA_DATA_DIR         = "/data"
        LISSANDRA_PUBLIC_URL       = var.public_url
        LISSANDRA_EVENT_BUFFER     = var.event_buffer
        LISSANDRA_LOG_LEVEL        = var.log_level
        LISSANDRA_LOG_FORMAT       = "json"
        LISSANDRA_MAX_IMPORT_BYTES = var.max_import_mb * 1048576
        LISSANDRA_IMPORT_MEMORY_MB = var.import_memory_mb
        # Let the Go runtime collect garbage before the task hits its limit.
        GOMEMLIMIT = "${floor(var.memory * 0.8)}MiB"
        # Engine addresses, rendered by the template below.
        LISSANDRA_ENGINE_URL_FILE = "$${NOMAD_TASK_DIR}/engine_urls"
      }

      # Secrets come from the Nomad Variable at nomad/jobs/lissandra, which
      # this job can read through its workload identity. Every key becomes an
      # environment variable, so besides LISSANDRA_ADMIN_TOKEN you can add
      # e.g. LISSANDRA_ENGINE_API_KEY (the engine's bearer key, which the
      # default engine settings read), SMTP_PASSWORD or SLACK_WEBHOOK_URL and
      # refer to them in the UI as ${env:SMTP_PASSWORD}. Changing the
      # variable restarts the task with the new values.
      template {
        destination = "${NOMAD_SECRETS_DIR}/lissandra.env"
        env         = true
        change_mode = "restart"
        data        = <<-EOT
          {{- with nomadVar "nomad/jobs/lissandra" -}}
          {{- range $k, $v := . }}
          {{ $k }}={{ $v | toJSON }}
          {{- end }}
          {{- end }}
        EOT
      }

      # Where the Laya engine is. The default engine settings read
      # ${env:LISSANDRA_ENGINE_URL}; it is unset here (don't add it to the
      # variable above, it would win), so Lissandra reads the file named by
      # LISSANDRA_ENGINE_URL_FILE instead, and re-reads it every 30 s. Engine
      # instances coming and going therefore never restart Lissandra (which
      # would empty its in-memory events). While no instance is registered,
      # Lissandra runs on rules alone.
      template {
        destination = "local/engine_urls"
        change_mode = "noop"
        data        = local.engine_urls
      }

      resources {
        cpu    = var.cpu
        memory = var.memory
      }

      # Let service discovery drop the instance before it stops. On SIGTERM
      # Lissandra drains for up to 28 s: it finishes the events it accepted
      # and delivers queued notifications. kill_timeout covers that; Nomad
      # clients cap it at their max_kill_timeout (30 s by default).
      shutdown_delay = "5s"
      kill_timeout   = "30s"
    }
  }
}
