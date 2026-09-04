# syntax=docker/dockerfile:1
#
# node-agent image — the "cluster-agent": a node-agent running the KUBERNETES
# driver, deployed as a Deployment inside the cluster whose pods it manages.
#
# run-node-agent.sh says the node-agent cannot be containerised, and for the
# Firecracker driver that is still true: it creates tap devices, writes nftables
# rules, execs the jailer and needs /dev/kvm. The kubernetes driver has none of
# those needs — it talks to an API server and dials pod IPs — which is exactly
# what makes this image possible. Do NOT run the firecracker driver from it.
#
# Build context is the REPO ROOT (the node-agent imports its own api package
# through the module path):
#
#   docker build -f deploy/node-agent/node-agent.Dockerfile \
#     -t ghcr.io/tavon-ai/proteos-node-agent:dev .
#
# See deploy/kubernetes/ for the manifests that run it.

FROM golang:1.26 AS build
WORKDIR /src
# GOWORK=off: the node-agent is a standalone module and imports nothing from the
# other workspace modules, so only nodeagent/ needs to be in the context.
# GOTOOLCHAIN=auto because the golang:1.26 tag can trail go.mod's exact patch.
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-trimpath GOWORK=off GOTOOLCHAIN=auto

COPY nodeagent/go.mod nodeagent/go.sum ./nodeagent/
RUN cd nodeagent && go mod download

COPY nodeagent/ ./nodeagent/
RUN cd nodeagent && go build -ldflags="-s -w" -o /out/node-agent ./cmd/nodeagent

# nonroot: unlike the host deployment, the cluster-agent needs no privileges at
# all. It reads a ServiceAccount token, calls the API server, and opens TCP
# connections to pod IPs.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/node-agent /usr/local/bin/proteos-node-agent
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/proteos-node-agent"]
