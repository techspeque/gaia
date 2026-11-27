-- Registered machines (OVERVIEW §2.2). Times are Unix nanoseconds, UTC.
CREATE TABLE nodes (
    id               TEXT    NOT NULL PRIMARY KEY,
    name             TEXT    NOT NULL UNIQUE,
    tailnet_identity TEXT    NOT NULL UNIQUE,
    os               TEXT    NOT NULL,
    arch             TEXT    NOT NULL,
    gaia_version     TEXT    NOT NULL,
    api_version      INTEGER NOT NULL,
    created_at       INTEGER NOT NULL,
    last_seen        INTEGER NOT NULL
) STRICT;
