-- The tailnet name the control server assigned each node, as the host saw
-- it (its MagicDNS short name). Empty when unknown: local-only mode, or a
-- row from before this column.
ALTER TABLE nodes ADD COLUMN tailnet_name TEXT NOT NULL DEFAULT '';
