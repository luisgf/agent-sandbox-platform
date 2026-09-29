-- Node enrollment / mTLS metadata + agent callback endpoint for exec dataplane.
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS agent_endpoint text NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS cert_fingerprint text NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS enrolled_at timestamptz;
CREATE INDEX IF NOT EXISTS nodes_cert_fingerprint_idx ON nodes (cert_fingerprint) WHERE cert_fingerprint <> '';
