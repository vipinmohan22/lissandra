# Laya: log ingestion, classification and alerting on Nomad.
#
#   nomad volume create deploy/nomad/laya-volume.hcl          # once
#   nomad var put nomad/jobs/laya LAYA_ADMIN_TOKEN="$(openssl rand -hex 24)"
#   nomad job run -var-file=deploy/nomad/laya.vars.hcl deploy/nomad/laya.nomad.hcl
#
# See deploy/nomad/README.md for details.

variable "datacenters" {
  description = "Datacenters Laya may run in."
  type        = list(string)
  default     = ["*"]
}

variable "namespace" {
  description = "Nomad namespace for the job."
  type        = string
  default     = "default"
}

variable "image" {
  description = "Laya container image."
  type        = string
  default     = "ghcr.io/vipinmohan22/laya:latest"
}

variable "volume_name" {
  description = "Host volume holding Laya's config (dynamic volume from laya-volume.hcl, or a static host_volume in the client config)."
  type        = string
  default     = "laya-data"
}

variable "public_url" {
  description = "External base URL of Laya (e.g. https://laya.example.com). Used for ingest URLs in the UI and links in notifications."
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
  description = "How many recent events Laya keeps in memory for the UI."
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

job "laya" {
  datacenters = var.datacenters
  namespace   = var.namespace
  type        = "service"

  group "laya" {
    # Laya keeps its configuration in a single file on the volume, so it runs
    # as exactly one instance. Nomad reschedules it if the node fails.
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
      name     = "laya"
      port     = "http"
      provider = var.service_provider
      tags     = concat(["laya", "logs", "metrics"], var.service_tags)

      check {
        name     = "laya-http"
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

    task "laya" {
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
        LAYA_LISTEN           = ":8080"
        LAYA_DATA_DIR         = "/data"
        LAYA_PUBLIC_URL       = var.public_url
        LAYA_EVENT_BUFFER     = var.event_buffer
        LAYA_LOG_LEVEL        = var.log_level
        LAYA_LOG_FORMAT       = "json"
        LAYA_MAX_IMPORT_BYTES = var.max_import_mb * 1048576
        LAYA_IMPORT_MEMORY_MB = var.import_memory_mb
        # Let the Go runtime collect garbage before the task hits its limit.
        GOMEMLIMIT = "${floor(var.memory * 0.8)}MiB"
      }

      # Secrets come from the Nomad Variable at nomad/jobs/laya, which this
      # job can read through its workload identity. Every key becomes an
      # environment variable, so besides LAYA_ADMIN_TOKEN you can add e.g.
      # ANTHROPIC_API_KEY, SMTP_PASSWORD or SLACK_WEBHOOK_URL and refer to
      # them in the UI as ${env:SMTP_PASSWORD}. Changing the variable
      # restarts the task with the new values.
      template {
        destination = "${NOMAD_SECRETS_DIR}/laya.env"
        env         = true
        change_mode = "restart"
        data        = <<-EOT
          {{- with nomadVar "nomad/jobs/laya" -}}
          {{- range $k, $v := . }}
          {{ $k }}={{ $v | toJSON }}
          {{- end }}
          {{- end }}
        EOT
      }

      resources {
        cpu    = var.cpu
        memory = var.memory
      }

      # Give in-flight notifications time to finish and let service
      # discovery drop the instance before it stops.
      shutdown_delay = "5s"
      kill_timeout   = "20s"
    }
  }
}
