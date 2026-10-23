# Dynamic host volume for Lissandra's configuration (Nomad 1.10+).
#
#   nomad volume create deploy/nomad/lissandra-volume.hcl
#
# The built-in "mkdir" plugin creates a directory under the client's
# host_volumes_dir, owned by the container's non-root user (65532).
# Pin it to a node pool or node with a constraint if you need to.

name      = "lissandra-data"
type      = "host"
plugin_id = "mkdir"

capability {
  access_mode     = "single-node-single-writer"
  attachment_mode = "file-system"
}

parameters {
  mode = "0700"
  uid  = 65532
  gid  = 65532
}
