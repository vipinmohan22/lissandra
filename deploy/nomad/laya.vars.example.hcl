# Copy to laya.vars.hcl and adjust, then:
#   nomad job run -var-file=deploy/nomad/laya.vars.hcl deploy/nomad/laya.nomad.hcl

datacenters = ["dc1"]
image       = "ghcr.io/vipinmohan22/laya:v0.1.0"
public_url  = "https://laya.example.com"

# "consul" if you use Consul for service discovery.
service_provider = "nomad"

# Example: expose through Traefik reading Nomad service discovery.
service_tags = [
  "traefik.enable=true",
  "traefik.http.routers.laya.rule=Host(`laya.example.com`)",
  "traefik.http.routers.laya.entrypoints=websecure",
  "traefik.http.routers.laya.tls=true",
]

# Or skip the load balancer and pin a host port:
# http_port = 8080

memory       = 384
event_buffer = 10000
