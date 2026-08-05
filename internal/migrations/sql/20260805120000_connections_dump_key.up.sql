-- Where this connection's session capture ended up in blob storage.
--
-- Empty means "not uploaded": either uploads are disabled (DBB_DUMP_UPLOAD_URL
-- unset, the default) or the capture is still in the local spool. The API
-- resolves a capture local-spool-first and falls back to this key, so it can
-- serve a capture written by another replica without ever LISTing the bucket —
-- a lookup by connection UID alone has no other way to know which instance
-- wrote the object, or on which day.
ALTER TABLE connections ADD COLUMN IF NOT EXISTS dump_key TEXT NOT NULL DEFAULT '';
