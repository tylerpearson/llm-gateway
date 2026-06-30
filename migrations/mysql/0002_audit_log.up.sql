CREATE TABLE audit_log (
    id         VARCHAR(36)  NOT NULL,
    actor      VARCHAR(255) NOT NULL,
    action     VARCHAR(64)  NOT NULL,
    target     VARCHAR(255) NOT NULL,
    details    TEXT         NULL,
    created_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    KEY idx_audit_log_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
