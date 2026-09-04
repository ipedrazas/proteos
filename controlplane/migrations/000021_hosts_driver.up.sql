-- K3 (Kubernetes backend): record which backend each host's node-agent runs.
--
-- Until now every host was a KVM box running Firecracker, so the control plane
-- could assume one machine shape fleet-wide. A cluster-agent
-- (PROTEOS_AGENT_DRIVER=kubernetes) is registered as an ordinary host row but
-- differs in two ways the control plane must know about before it acts: it boots
-- an OCI image rather than a kernel + ext4 rootfs, and it cannot hibernate
-- (a pod has no RAM to snapshot). Both decisions are made before any agent call
-- — placement at create, stop mode at stop — so the kind has to be readable from
-- Postgres rather than discovered by asking the agent.
--
-- Defaulting to 'firecracker' keeps every existing row and every deployment that
-- does not set PROTEOS_HOST_DRIVER/PROTEOS_HOSTS[].driver exactly as it was.
ALTER TABLE hosts ADD COLUMN driver text NOT NULL DEFAULT 'firecracker'
    CHECK (driver IN ('firecracker', 'kubernetes', 'dev'));
