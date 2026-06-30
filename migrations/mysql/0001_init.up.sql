CREATE TABLE teams (
    id         VARCHAR(36)  NOT NULL,
    name       VARCHAR(255) NOT NULL,
    created_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    UNIQUE KEY uq_teams_name (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE virtual_keys (
    id            VARCHAR(36)  NOT NULL,
    team_id       VARCHAR(36)  NOT NULL,
    name          VARCHAR(255) NOT NULL,
    key_hash      CHAR(64)     NOT NULL,
    default_alias VARCHAR(64)  NULL,
    disabled      TINYINT(1)   NOT NULL DEFAULT 0,
    created_at    TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    UNIQUE KEY uq_virtual_keys_hash (key_hash),
    KEY idx_virtual_keys_team (team_id),
    CONSTRAINT fk_virtual_keys_team FOREIGN KEY (team_id) REFERENCES teams (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
