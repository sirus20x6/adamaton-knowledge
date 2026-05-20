-- Reverse the cut-1 backfill. Leaves cut-2+ writes untouched by
-- scoping to provenance='backfill', and clears the canonical entities
-- only when no other rows reference them (CASCADE would be too greedy).

DELETE FROM deepresearch.entity_relations
 WHERE provenance = 'backfill';

DELETE FROM deepresearch.entities e
 WHERE NOT EXISTS (
   SELECT 1 FROM deepresearch.entity_relations er
    WHERE er.subject_id = e.id OR er.object_id = e.id
 )
 AND NOT EXISTS (
   SELECT 1 FROM deepresearch.episode_mentions em WHERE em.entity_id = e.id
 )
 AND NOT EXISTS (
   SELECT 1 FROM deepresearch.document_mentions dm WHERE dm.entity_id = e.id
 );
