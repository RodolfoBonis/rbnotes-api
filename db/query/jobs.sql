-- CountJobsByKindStatus alimenta o gauge jobs_queue (retrato da fila no ai-worker).
-- name: CountJobsByKindStatus :many
SELECT kind, status, count(*)::bigint AS n
FROM jobs
WHERE status IN ('queued', 'running', 'dead')
GROUP BY kind, status;
