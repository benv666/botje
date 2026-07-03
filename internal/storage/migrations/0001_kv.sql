-- the generic module kv store: the 2007 Storage_MySQL design, delivered
CREATE TABLE kv (
    namespace  text        NOT NULL,
    name       text        NOT NULL,
    value      jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, name)
);
