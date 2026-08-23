CREATE TABLE retrieval_audits (
    tenant_id text NOT NULL, id text NOT NULL, session_id text NOT NULL, turn_id text NOT NULL, run_id text NOT NULL,
    knowledge_base_id text NOT NULL, index_version_id text NOT NULL, original_query text NOT NULL, retrieval_query text NOT NULL,
    degraded_route text, candidate_count integer NOT NULL, selected_count integer NOT NULL,
    created_at timestamptz NOT NULL, expires_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id,id),
    CHECK (tenant_id ~ '^ten_[A-Za-z0-9_-]{4,124}$' AND id ~ '^ret_[A-Za-z0-9_-]{4,124}$'),
    CHECK (session_id ~ '^ses_[A-Za-z0-9_-]{4,124}$' AND turn_id ~ '^trn_[A-Za-z0-9_-]{4,124}$' AND run_id ~ '^run_[A-Za-z0-9_-]{4,124}$'),
    CHECK (knowledge_base_id ~ '^kbs_[A-Za-z0-9_-]{4,124}$' AND index_version_id ~ '^ver_[A-Za-z0-9_-]{4,124}$'),
    CHECK (length(btrim(original_query)) > 0 AND length(btrim(retrieval_query)) > 0),
    CHECK (degraded_route IS NULL OR degraded_route IN ('VECTOR','BM25')),
    CHECK (candidate_count > 0 AND selected_count > 0 AND selected_count <= candidate_count),
    CHECK (expires_at >= created_at + interval '1 hour' AND expires_at <= created_at + interval '365 days'),
    FOREIGN KEY (tenant_id,run_id) REFERENCES runs(tenant_id,id) ON DELETE CASCADE
);

CREATE TABLE retrieval_audit_candidates (
    tenant_id text NOT NULL, retrieval_id text NOT NULL, ordinal integer NOT NULL,
    document_id text NOT NULL, chunk_id text NOT NULL, vector_rank integer NOT NULL, bm25_rank integer NOT NULL,
    fused_score double precision NOT NULL, rerank_score double precision, selected boolean NOT NULL,
    artifact_id text, content_sha256 bytea,
    PRIMARY KEY (tenant_id,retrieval_id,ordinal), UNIQUE (tenant_id,retrieval_id,chunk_id),
    CHECK (ordinal > 0 AND document_id ~ '^doc_[A-Za-z0-9_-]{4,124}$' AND chunk_id ~ '^chk_[A-Za-z0-9_-]{4,124}$'),
    CHECK (vector_rank >= 0 AND bm25_rank >= 0 AND (vector_rank > 0 OR bm25_rank > 0)),
    CHECK (fused_score > '-Infinity'::float8 AND fused_score < 'Infinity'::float8
        AND (rerank_score IS NULL OR (rerank_score > '-Infinity'::float8 AND rerank_score < 'Infinity'::float8))),
    CHECK ((selected AND artifact_id IS NOT NULL AND octet_length(content_sha256)=32) OR (NOT selected AND artifact_id IS NULL AND content_sha256 IS NULL)),
    CHECK (artifact_id IS NULL OR artifact_id ~ '^art_[A-Za-z0-9_-]{4,124}$'),
    FOREIGN KEY (tenant_id,retrieval_id) REFERENCES retrieval_audits(tenant_id,id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id,artifact_id) REFERENCES artifact_metadata(tenant_id,id) DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX retrieval_audits_expiry_idx ON retrieval_audits(tenant_id,expires_at,id);
CREATE INDEX retrieval_candidates_selected_idx ON retrieval_audit_candidates(tenant_id,retrieval_id,ordinal) WHERE selected;
