-- Whether the node forces its guests through the egress proxy (nft rules applied
-- in enforce mode), as the node said on its last register. A node that predates
-- the field, or runs without the proxy and the rules, is not enforcing.
ALTER TABLE nodes
    ADD COLUMN IF NOT EXISTS egress_enforced boolean NOT NULL DEFAULT false;
