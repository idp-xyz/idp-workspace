-- idp-workspace schema v6 (ADR-0001 D29): the web sources of a finished run, kept on its inbox item. Both columns
-- are NULL for every item that has none (all items before this migration, and every run that read no web page), so
-- the existing rows, policies and grants are unchanged: whoever may see an item sees its sources, nobody else.
-- sources: [{n, title, url, fetchedAt?, sha256?, cited}], ordered by n, at most 100 (the follower checks every field
-- before it writes; the checks here are the last line). unverified_citations: the numbers the answer cites that are
-- no source.
ALTER TABLE inbox_items
  ADD COLUMN sources jsonb CHECK (sources IS NULL OR (jsonb_typeof(sources) = 'array' AND jsonb_array_length(sources) BETWEEN 1 AND 100 AND pg_column_size(sources) <= 1048576)),
  ADD COLUMN unverified_citations integer[] CHECK (unverified_citations IS NULL OR cardinality(unverified_citations) BETWEEN 1 AND 100);
