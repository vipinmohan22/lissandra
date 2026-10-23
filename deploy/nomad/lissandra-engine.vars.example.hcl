# Copy to lissandra-engine.vars.hcl and adjust, then:
#   nomad job run -var-file=deploy/nomad/lissandra-engine.vars.hcl deploy/nomad/lissandra-engine.nomad.hcl

datacenters = ["dc1"]

# Built from the upstream Dockerfile and pushed to your registry (see README).
image = "registry.example.com/lissandra-engine:0.4.2"

# One instance answers about 1-2 new message patterns per second on 4 cores.
count  = 1
cores  = 4
memory = 3500

# Checkpoints to load. Add multilingual (and about 2 GB of memory) if a
# source's engine checkpoint is set to it.
models = "english"

# Pin the checkpoint weights (Hub commit of convaiinnovations/laya).
revision = "7b928d828b7b0e022f929d9bd2e44165aa270148"

# After the first start has filled the cache, keep the engine off the network:
# offline = true

# NVIDIA GPU (image built with --build-arg TORCH_INDEX=cu130):
# gpu   = true
# cores = 2
