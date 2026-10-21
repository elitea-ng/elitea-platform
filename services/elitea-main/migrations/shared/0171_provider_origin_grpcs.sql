-- 0171_provider_origin_grpcs.sql — a registered provider origin may be the
-- provider's gRPC listener, grpcs://host:port (#1359).
--
-- WHY. The facades reach their providers (DeepWiki, Inventory) over gRPC on
-- mTLS now, not HTTP: the registrar reads the descriptor with
-- ProviderService.Describe and records the origin it dialled, which is
-- grpcs://host:port. 0107's CHECK admitted only http(s) origins, so every
-- boot-time registration was refused (SQLSTATE 23514) and the admission plane
-- never heard from a composed facade: the Service Descriptors page listed
-- nothing on a stack that serves both providers.
--
-- The shape stays what 0107 normalises to — scheme, host, optional port, no
-- path, no credentials — with one more scheme. No data changes: every stored
-- origin already satisfies the wider check.
ALTER TABLE provider_hub.provider_origin_registration
    DROP CONSTRAINT provider_origin_registration_origin_absolute;

ALTER TABLE provider_hub.provider_origin_registration
    ADD CONSTRAINT provider_origin_registration_origin_absolute
        CHECK (origin ~ '^(https?|grpcs)://[^/[:space:]]+$');
