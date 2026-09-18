-- Schema 7: repair coverage windows written by the older DWS adapter.
--
-- DWS uses stopReason=source_complete with no continuation cursor to mean the
-- requested source range was exhausted successfully. Older memgov binaries
-- persisted that result as an incomplete window, so status kept reporting a
-- gap even though every available page had been read. Preserve real failures
-- and capped windows; only the exact source-complete/no-cursor shape is closed.
UPDATE coverage_windows
SET complete=1, gap=''
WHERE complete=0 AND stop_reason='source_complete' AND cursor='';

UPDATE channel_watermarks
SET gap_unresolved=CASE WHEN EXISTS (
  SELECT 1 FROM coverage_windows cw
  WHERE cw.channel_id=channel_watermarks.channel_id
    AND cw.conversation_id=channel_watermarks.conversation_id
    AND cw.complete=0
) THEN 1 ELSE 0 END;
