# Laya engine: laya-serve (github.com/NandhaKishorM/laya), the decision model
# Laya asks to classify what its rules leave uncategorized.
#
#   docker build -t registry.example.com/laya-engine:0.4.2 \
#     https://github.com/NandhaKishorM/laya.git#v0.4.2      # no public image exists
#   docker push registry.example.com/laya-engine:0.4.2
#   nomad volume create deploy/nomad/laya-engine-volume.hcl # once per node (model cache)
#   nomad var put nomad/jobs/laya-engine LAYA_API_KEY="$(openssl rand -hex 32)"
#   nomad job run -var-file=deploy/nomad/laya-engine.vars.hcl deploy/nomad/laya-engine.nomad.hcl
#
# Laya's own job (laya.nomad.hcl) finds the instances through service
# discovery. Give it the same key as LAYA_ENGINE_API_KEY in nomad/jobs/laya.
# See deploy/nomad/README.md for details.

variable "datacenters" {
  description = "Datacenters the engine may run in."
  type        = list(string)
  default     = ["*"]
}

variable "namespace" {
  description = "Nomad namespace for the job. Use the same one as the laya job, so it can discover the engine."
  type        = string
  default     = "default"
}

variable "image" {
  description = "laya-serve image built from the upstream Dockerfile (no public image is published). Build it with --build-arg TORCH_INDEX=cu130 when gpu = true."
  type        = string
  default     = "laya-engine:0.4.2"
}

variable "count" {
  description = "Number of engine instances. Laya spreads requests over all of them and fails over between them."
  type        = number
  default     = 1
}

variable "cores" {
  description = "Dedicated CPU cores per instance. Also sets LAYA_THREADS and OMP_NUM_THREADS."
  type        = number
  default     = 4
}

variable "memory" {
  description = "Memory in MB reserved per instance. The english checkpoint uses 2.1-2.4 GB resident and peaks near 3 GB while loading; add about 2 GB for each extra checkpoint in models."
  type        = number
  default     = 3500
}

variable "memory_max" {
  description = "Memory limit in MB (needs memory oversubscription enabled in the scheduler configuration; otherwise memory is the limit)."
  type        = number
  default     = 4500
}

variable "gpu" {
  description = "Run on an NVIDIA GPU: requests one nvidia/gpu device and sets LAYA_DEVICE=cuda. The image must be built with --build-arg TORCH_INDEX=cu130."
  type        = bool
  default     = false
}

variable "models" {
  description = "LAYA_MODELS: checkpoints loaded at startup, comma-separated (english, multilingual, typed-decisions, or names from extra_models). Include every checkpoint Laya's settings or sources ask for."
  type        = string
  default     = "english"
}

variable "default_model" {
  description = "LAYA_DEFAULT_MODEL. Laya always names the checkpoint it wants, so this only matters to other clients."
  type        = string
  default     = "english"
}

variable "max_loaded" {
  description = "LAYA_MAX_LOADED: checkpoints kept resident at once. 0 means one per entry in models."
  type        = number
  default     = 0
}

variable "extra_models" {
  description = "LAYA_EXTRA_MODELS: JSON object {\"name\": source} registering fine-tuned checkpoints (a Hub repo id, a directory inside the container, or [\"repo\", \"subfolder\"]). Empty for none."
  type        = string
  default     = ""
}

variable "revision" {
  description = "LAYA_REVISION: Hub commit every checkpoint is loaded at. The default is the main branch of convaiinnovations/laya when this job was written. Empty follows main (or the ref baked into the image)."
  type        = string
  default     = "7b928d828b7b0e022f929d9bd2e44165aa270148"
}

variable "offline" {
  description = "HF_HUB_OFFLINE: never contact huggingface.co and load only what is already in the cache (or baked into the image)."
  type        = bool
  default     = false
}

variable "cache_volume" {
  description = "Host volume for the Hugging Face model cache, mounted at /home/laya/.cache/huggingface. Empty: no volume, so weights baked into the image are used, or the checkpoint is downloaded again whenever the task starts."
  type        = string
  default     = "laya-engine-cache"
}

variable "port" {
  description = "Static host port for laya-serve. 0 picks a dynamic port (Laya finds it through service discovery)."
  type        = number
  default     = 0
}

variable "service_provider" {
  description = "Service discovery provider: \"nomad\" (built in) or \"consul\". Use the same as the laya job."
  type        = string
  default     = "nomad"
}

variable "service_tags" {
  description = "Extra service tags."
  type        = list(string)
  default     = []
}

variable "log_level" {
  description = "LAYA_LOG_LEVEL (uvicorn): critical, error, warning, info or debug. info logs every request."
  type        = string
  default     = "warning"
}

variable "startup_minutes" {
  description = "How long a starting instance may take to become healthy. The first start downloads the checkpoint (about 0.85 GB for english) and loads it."
  type        = number
  default     = 10
}

locals {
  max_loaded = var.max_loaded > 0 ? var.max_loaded : length(compact(split(",", var.models)))
}

job "laya-engine" {
  datacenters = var.datacenters
  namespace   = var.namespace
  type        = "service"

  group "engine" {
    count = var.count

    network {
      port "http" {
        static = var.port
        to     = 8000
      }
    }

    dynamic "volume" {
      for_each = var.cache_volume == "" ? [] : [var.cache_volume]
      labels   = ["cache"]
      content {
        type            = "host"
        source          = volume.value
        read_only       = false
        access_mode     = "single-node-single-writer"
        attachment_mode = "file-system"
      }
    }

    service {
      name     = "laya-engine"
      port     = "http"
      provider = var.service_provider
      tags     = concat(["laya-engine"], var.service_tags)

      # laya-serve starts listening only once its checkpoints are loaded, so
      # a passing check means the engine can answer. Without the API key,
      # /health reports liveness only, which is all this check needs.
      check {
        name     = "laya-engine-health"
        type     = "http"
        path     = "/health"
        interval = "10s"
        timeout  = "5s"

        # The first start downloads and loads the checkpoint; restart only
        # an instance that stops answering after that. After a restart the
        # grace period counts from the restart signal, and stopping the old
        # process takes up to 2 minutes of it: shutdown_delay (5 s), the
        # kill (kill_timeout, capped at 30 s by most clients, 60 s at most)
        # and the restart delay (30 s, plus up to 25% jitter).
        check_restart {
          limit = 3
          grace = "${var.startup_minutes * 60 + 120}s"
        }
      }
    }

    restart {
      attempts = 3
      interval = "30m"
      delay    = "30s"
      mode     = "delay"
    }

    update {
      max_parallel      = 1
      health_check      = "checks"
      min_healthy_time  = "10s"
      healthy_deadline  = "${var.startup_minutes + 5}m"
      progress_deadline = "${var.startup_minutes + 15}m"
      auto_revert       = true
    }

    reschedule {
      delay          = "1m"
      delay_function = "exponential"
      max_delay      = "15m"
      unlimited      = true
    }

    task "laya-serve" {
      driver = "docker"

      config {
        image = var.image
        ports = ["http"]
        # The image's ENTRYPOINT (/opt/laya/entrypoint.py) turns
        # LAYA_API_KEY_FILE into LAYA_API_KEY, then runs this command. Its
        # default command is a one-shot quickstart, not the server.
        command = "laya-serve"
      }

      dynamic "volume_mount" {
        for_each = var.cache_volume == "" ? [] : ["cache"]
        content {
          volume      = volume_mount.value
          destination = "/home/laya/.cache/huggingface"
        }
      }

      env {
        LAYA_HOST          = "0.0.0.0"
        LAYA_PORT          = "${NOMAD_PORT_http}"
        LAYA_PRELOAD       = "1"
        LAYA_MODELS        = var.models
        LAYA_DEFAULT_MODEL = var.default_model
        LAYA_MAX_LOADED    = local.max_loaded
        LAYA_EXTRA_MODELS  = var.extra_models
        LAYA_REVISION      = var.revision
        LAYA_DEVICE        = var.gpu ? "cuda" : "cpu" # never "auto"
        LAYA_THREADS       = var.cores
        OMP_NUM_THREADS    = var.cores
        LAYA_LOG_LEVEL     = var.log_level
        LAYA_API_KEY_FILE  = "${NOMAD_SECRETS_DIR}/laya_api_key"
        HF_HUB_OFFLINE     = var.offline ? "1" : "0"
        # torch 2.14 otherwise compiles Triton kernels on the first CUDA
        # inference, which needs a C compiler the image does not have.
        TORCH_DISABLE_NATIVE_JIT = "1"
        TOKENIZERS_PARALLELISM   = "false"
        HF_HUB_DISABLE_TELEMETRY = "1"
      }

      # The bearer key Laya must send, from the Nomad Variable at
      # nomad/jobs/laya-engine (key LAYA_API_KEY), which this job reads
      # through its workload identity. The file is readable only by the
      # image's user (10001). Changing the key restarts the engine.
      template {
        destination = "${NOMAD_SECRETS_DIR}/laya_api_key"
        change_mode = "restart"
        uid         = 10001
        gid         = 10001
        perms       = "0400"
        data        = <<-EOT
          {{- with nomadVar "nomad/jobs/laya-engine" }}{{ .LAYA_API_KEY }}{{ end -}}
        EOT
      }

      # Any other key in the variable becomes an environment variable, e.g.
      # HF_TOKEN for a fine-tuned checkpoint in a private Hub repository.
      template {
        destination = "${NOMAD_SECRETS_DIR}/engine.env"
        env         = true
        change_mode = "restart"
        data        = <<-EOT
          {{- with nomadVar "nomad/jobs/laya-engine" -}}
          {{- range $k, $v := . }}{{ if ne $k "LAYA_API_KEY" }}
          {{ $k }}={{ $v | toJSON }}
          {{- end }}{{ end }}
          {{- end }}
        EOT
      }

      resources {
        cores      = var.cores
        memory     = var.memory
        memory_max = var.memory_max

        # One NVIDIA GPU (needs the nvidia device plugin on the client). The
        # docker driver runs the container with the nvidia runtime for it.
        dynamic "device" {
          for_each = var.gpu ? ["nvidia/gpu"] : []
          labels   = [device.value]
          content {
            count = 1
          }
        }
      }

      # Let service discovery drop the instance, then give requests in
      # flight time to finish: laya-serve waits for them on SIGTERM.
      shutdown_delay = "5s"
      kill_signal    = "SIGTERM"
      kill_timeout   = "60s"
    }
  }
}
