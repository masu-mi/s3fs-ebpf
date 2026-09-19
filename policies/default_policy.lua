-- default_policy.lua — cache policy stub.
-- Contract: return a table describing how to cache the object.
--   action: "DEFAULT" | "WRITE_BACK" | "CACHE_PERMANENT" | "PASSTHROUGH"
--   ttl:    seconds (int)
--   prefetch: bool

function on_cache_check(path, s3_metadata)
    return {
        action   = "DEFAULT",
        ttl      = 60,
        prefetch = false,
    }
end
