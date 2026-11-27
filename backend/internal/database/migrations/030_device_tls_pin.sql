-- +goose Up

-- devices.pin_cert_sha256: the fingerprint of the certificate a device's
-- api-ssl service presents — the SHA-256 of the certificate's DER encoding as
-- lower-case hex, which is what RouterOS prints as "fingerprint" in
-- /certificate print detail. An admin pastes it into the device; every TLS
-- session compares the certificate the peer presents with it before any
-- credential is sent. NULL = no pin: a TLS device is then not logged in to
-- at all, a plain-API device is unaffected.
--
-- Additive and nullable (M0 schema rule): the previous release's binary
-- ignores the column.
ALTER TABLE devices ADD COLUMN pin_cert_sha256 TEXT;

-- +goose Down
ALTER TABLE devices DROP COLUMN pin_cert_sha256;
