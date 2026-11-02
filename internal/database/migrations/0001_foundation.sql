-- Phase 1 foundation: shared helpers used by every domain table.
--
-- Conventions for later migrations:
--   * Primary keys:  id bigint GENERATED ALWAYS AS IDENTITY
--   * Timestamps:    created_at / updated_at timestamptz NOT NULL DEFAULT now()
--   * Money:         bigint in rupiah (no floating point)
--   * Every table with updated_at gets the trigger below.

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$;
