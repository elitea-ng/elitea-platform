-- The standalone 'bm25' branch of the BM25 statistics is no longer written
-- or read (ADR-0031 phase D0). Only the 'fts' branch, the one the live
-- hybrid search ranks with, remains. This removes the rows existing wikis
-- carry for the dead branch (millions of postings for a large repository).
--
-- The tables stay: 'fts' uses them. The build space's staging tables
-- `deepwiki_build.bm25_docs` and `bm25_postings` are left in place too
-- (nothing writes them any more), so a replica of the previous release that
-- is still running during a rolling deploy keeps working.
DELETE FROM wiki_bm25_postings WHERE branch = 'bm25';
DELETE FROM wiki_bm25_terms    WHERE branch = 'bm25';
DELETE FROM wiki_bm25_docs     WHERE branch = 'bm25';
DELETE FROM wiki_bm25_meta     WHERE branch = 'bm25';
