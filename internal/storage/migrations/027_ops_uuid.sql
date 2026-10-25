-- ops_uuid parses an id from a URL or token: NULL (matches nothing) if it isn't a uuid. Queries use
-- "id = ops_uuid($1)" instead of "id::text = $1", which kept the same not-found semantics but
-- defeated the indexes, so every lookup scanned the table.
CREATE FUNCTION ops_uuid(t text) RETURNS uuid LANGUAGE plpgsql IMMUTABLE STRICT PARALLEL SAFE AS $$
BEGIN
    RETURN t::uuid;
EXCEPTION WHEN invalid_text_representation THEN
    RETURN NULL;
END $$;
