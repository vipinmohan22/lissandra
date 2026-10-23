# Copy to lissandra.vars.hcl and adjust, then:
#   nomad job run -var-file=deploy/nomad/lissandra.vars.hcl deploy/nomad/lissandra.nomad.hcl

datacenters = ["dc1"]
image       = "ghcr.io/vipinmohan22/lissandra:v0.1.0"
public_url  = "https://lissandra.example.com"

# "consul" if you use Consul for service discovery.
service_provider = "nomad"

# Example: expose through Traefik reading Nomad service discovery.
service_tags = [
  "traefik.enable=true",
  "traefik.http.routers.lissandra.rule=Host(`lissandra.example.com`)",
  "traefik.http.routers.lissandra.entrypoints=websecure",
  "traefik.http.routers.lissandra.tls=true",
]

# Or skip the load balancer and pin a host port:
# http_port = 8080

memory       = 384
event_buffer = 10000
