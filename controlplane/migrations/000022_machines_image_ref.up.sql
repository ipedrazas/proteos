-- K3 (Kubernetes backend): the OCI image a machine boots from, pinned at create
-- time the way kernel_ref/rootfs_ref already are.
--
-- Nullable rather than NOT NULL DEFAULT '': a machine created from a
-- Firecracker-only template has no image, and "not applicable" is a different
-- statement from "the empty image". Every machine that predates this column is
-- one of those.
ALTER TABLE machines ADD COLUMN image_ref text;
