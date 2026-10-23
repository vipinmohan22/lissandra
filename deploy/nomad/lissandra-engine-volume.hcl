# Dynamic host volume for the engine's model cache (Nomad 1.10+).
#
#   nomad volume create deploy/nomad/lissandra-engine-volume.hcl
#
# The built-in "mkdir" plugin creates a directory under the client's
# host_volumes_dir, owned by the laya-serve image's user (10001). Checkpoints
# downloaded from huggingface.co are kept here, so restarts and upgrades
# load them from disk (english is about 0.85 GB, multilingual 0.68 GB).
#
# Each engine instance needs this volume on its own node. For count > 1, run
# the command once per instance: each run places the volume on a node that
# doesn't have one yet (set node_id, node_pool or a constraint to choose).

name      = "lissandra-engine-cache"
type      = "host"
plugin_id = "mkdir"
# node_id = "<node id from nomad node status>"

capability {
  access_mode     = "single-node-single-writer"
  attachment_mode = "file-system"
}

parameters {
  mode = "0700"
  uid  = 10001
  gid  = 10001
}
