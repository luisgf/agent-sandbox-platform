-- #132: two API keys must not share a secret.
--
-- LookupAPIKeyByHash finds the key of a request by the hash of its secret, so a
-- hash two rows share answered with whichever came first: a request could
-- authenticate as another key, of another tenant. The memory store, keyed by the
-- hash, kept one of the two and lost the other. The index also turns that lookup,
-- made on every authenticated request, from a scan of the table into a seek.
--
-- Keys that already share a secret cannot both keep it. The newest keeps working;
-- the older ones are revoked and given a hash nobody can present (the hash of
-- their old hash and their id), so the rows, and the audit trail, stay.
UPDATE api_keys a
SET secret_hash = encode(sha256(convert_to(a.secret_hash || a.id, 'UTF8')), 'hex'),
    revoked_at = COALESCE(a.revoked_at, now())
WHERE EXISTS (
    SELECT 1 FROM api_keys b
    WHERE b.secret_hash = a.secret_hash AND (b.created_at, b.id) > (a.created_at, a.id)
);

CREATE UNIQUE INDEX IF NOT EXISTS api_keys_secret_hash_key ON api_keys (secret_hash);
