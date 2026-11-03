-- Phase 2: customer profile (separate from login credentials) and addresses.

-- Phone numbers are stored normalized as Indonesian mobile numbers in E.164,
-- e.g. +6281234567890.

CREATE TABLE user_profiles (
    user_id    bigint      PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    first_name text        NOT NULL DEFAULT '',
    last_name  text        NOT NULL DEFAULT '',
    phone      text,
    gender     text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT user_profiles_phone_check  CHECK (phone ~ '^\+628[0-9]{7,11}$'),
    CONSTRAINT user_profiles_gender_check CHECK (gender IN ('LAKI_LAKI', 'PEREMPUAN'))
);

CREATE TRIGGER user_profiles_set_updated_at
    BEFORE UPDATE ON user_profiles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE user_addresses (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id        bigint           NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    label          text             NOT NULL,
    recipient_name text             NOT NULL,
    phone          text             NOT NULL,
    province       text             NOT NULL,
    city           text             NOT NULL,
    district       text             NOT NULL,
    village        text             NOT NULL,
    postal_code    text             NOT NULL,
    street_address text             NOT NULL,
    latitude       double precision,
    longitude      double precision,
    maps_url       text,
    is_primary     boolean          NOT NULL DEFAULT false,
    created_at     timestamptz      NOT NULL DEFAULT now(),
    updated_at     timestamptz      NOT NULL DEFAULT now(),
    CONSTRAINT user_addresses_phone_check       CHECK (phone ~ '^\+628[0-9]{7,11}$'),
    CONSTRAINT user_addresses_postal_code_check CHECK (postal_code ~ '^[0-9]{5}$'),
    CONSTRAINT user_addresses_latitude_check    CHECK (latitude BETWEEN -90 AND 90),
    CONSTRAINT user_addresses_longitude_check   CHECK (longitude BETWEEN -180 AND 180),
    CONSTRAINT user_addresses_coordinates_check CHECK ((latitude IS NULL) = (longitude IS NULL))
);

CREATE INDEX user_addresses_user_id_idx ON user_addresses (user_id);

-- At most one primary address per user, enforced by the database.
CREATE UNIQUE INDEX user_addresses_one_primary_idx ON user_addresses (user_id) WHERE is_primary;

CREATE TRIGGER user_addresses_set_updated_at
    BEFORE UPDATE ON user_addresses
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
