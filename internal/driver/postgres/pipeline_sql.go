package postgres

// The pipeline's lookups that a test shows Postgres answers from an index
// (pipeline_plan_test.go), named so the test reads the very text the driver
// runs. Each names the index it reads. A literal in a condition (a state, a
// boolean) is the one in the partial index's own WHERE, which is what lets
// the planner prove the index covers the rows asked for.
const (
	// storageUsedSQL asks whether a reception in progress names a storage:
	// receptions_by_storage.
	storageUsedSQL = `SELECT 1 FROM receptions WHERE storage = $1 AND NOT ended LIMIT 1`
	// receiverOfStorageSQL names a receiver that archives to a storage:
	// receivers_by_storage.
	receiverOfStorageSQL = `SELECT name FROM receivers WHERE storage = $1 ORDER BY name LIMIT 1`
	// receiversOnPortSQL reads the other receivers on a port:
	// receivers_by_port.
	receiversOnPortSQL = `SELECT ` + receiverColumns + ` FROM receivers WHERE port = $1 AND name <> $2`
	// receptionNamesReceiverSQL and quarantineNamesReceiverSQL ask whether a
	// record still names a receiver: receptions_by_receiver and
	// quarantine_by_receiver. Like every "is there one" lookup of a value
	// that many rows may share, it is planned for its value (inUse).
	receptionNamesReceiverSQL  = `SELECT 1 FROM receptions WHERE receiver = $1 LIMIT 1`
	quarantineNamesReceiverSQL = `SELECT 1 FROM quarantine WHERE receiver = $1 LIMIT 1`
	// importJobNamesReceiverSQL asks whether an import job, ended or not,
	// names a receiver: import_jobs_by_receiver.
	importJobNamesReceiverSQL = `SELECT 1 FROM import_jobs WHERE receiver = $1 LIMIT 1`
	// listStoragesSQL and listReceiversSQL page by name: each table's
	// primary key.
	listStoragesSQL  = `SELECT ` + storageColumns + ` FROM storages WHERE name > $1 ORDER BY name LIMIT $2`
	listReceiversSQL = `SELECT ` + receiverColumns + ` FROM receivers WHERE name > $1 ORDER BY name LIMIT $2`
	// openReceptionSQL finds the reception of a study receiving at a
	// receiver: receptions_open, the partial unique index that holds one per
	// receiver and study.
	openReceptionSQL = `SELECT ` + receptionColumns + ` FROM receptions
		WHERE receiver = $1 AND study_uid = $2 AND state = 'receiving'`
	// nextArrivalSQL reads a study's next arrival number: receptions_arrival,
	// read backward for its one highest entry.
	nextArrivalSQL = `SELECT coalesce(max(arrival), 0) + 1 FROM receptions WHERE study_uid = $1`
	// arrivalsSQL reads a study's latest arrivals: receptions_arrival,
	// backward.
	arrivalsSQL = `SELECT id, arrival, state, started FROM receptions WHERE study_uid = $1 ORDER BY arrival DESC LIMIT $2`
	// listReceivedSQL pages a reception's received files: the primary key.
	listReceivedSQL = `SELECT ` + receivedColumns + ` FROM received_files WHERE reception_id = $1 AND (sop_instance_uid, sha256) > ($2, $3)
		ORDER BY sop_instance_uid, sha256 LIMIT $4`
	// workByKeySQL finds the unit queued or leased under a unique key:
	// work_by_key.
	workByKeySQL = `SELECT ` + workColumns + ` FROM work WHERE unique_key = $1 AND state IN ('queued', 'leased')`
	// claimNextSQL takes the earliest due unit of a kind among those queued
	// or leased, skipping a unit another transaction holds: work_claimable,
	// which finished units, however many, are not in.
	claimNextSQL = `SELECT ` + workColumns + ` FROM work
		WHERE kind = $1 AND state IN ('queued', 'leased') AND due <= $2 ORDER BY due, id LIMIT 1 FOR NO KEY UPDATE SKIP LOCKED`
	// sessionLabelsSQL reads a study's session's labels and storage: the
	// sessions' unique index on study_uid, and the subjects' primary key.
	sessionLabelsSQL = `SELECT sub.label, s.label, s.storage, s.route FROM sessions AS s
		JOIN subjects AS sub ON sub.id = s.subject_id WHERE s.study_uid = $1`
	// scanLabelsSQL reads the labels of the scans of a list of series: the
	// scans' unique index on series_uid.
	scanLabelsSQL = `SELECT series_uid, label FROM scans WHERE series_uid = ANY($1::text[])`
	// receptionScansSQL reads the scans a reception confirmed instances in,
	// with its running totals of each: reception_scans' primary key, and
	// the scans' primary key.
	receptionScansSQL = `SELECT s.label, s.series_uid, r.instances, r.bytes FROM reception_scans AS r
		JOIN scans AS s ON s.id = r.scan_id WHERE r.reception_id = $1 ORDER BY s.label, s.id LIMIT $2`
	// storageHoldsSQL asks whether a storage holds an instance, which is
	// whether a scan on it holds one: scans_by_storage.
	storageHoldsSQL = `SELECT 1 FROM scans WHERE storage = $1 AND instances > 0 LIMIT 1`
	// sessionsOnStorageSQL asks whether a session names a storage, where
	// every later arrival of it is archived: sessions_by_storage.
	sessionsOnStorageSQL = `SELECT 1 FROM sessions WHERE storage = $1 LIMIT 1`
	// receptionsWithoutWorkSQL pages the receptions past receiving that have
	// not ended and have no live unit of work: one of the partial indexes of
	// the receptions not ended (receptions_in_progress, or
	// receptions_by_storage when the planner finds those few rows cheaper to
	// sort), and work_by_reception for each.
	receptionsWithoutWorkSQL = `SELECT ` + receptionColumns + ` FROM receptions AS p
		WHERE NOT p.ended AND p.state <> 'receiving' AND (p.started, p.id) > ($1, $2)
		AND NOT EXISTS (SELECT 1 FROM work AS w WHERE w.reception_id = p.id AND w.state IN ('queued', 'leased'))
		ORDER BY p.started, p.id LIMIT $3`
	// releaseClaimedSQL releases a reception's claims not confirmed:
	// instance_claims_by_reception. A held instance is in another table.
	releaseClaimedSQL = `DELETE FROM instance_claims WHERE reception_id = $1`
	// instanceRowsSQL reads what the catalog holds or a reception claims
	// under each of a batch's SOP Instance UIDs: the held instance with its
	// scan's study and series (the instances' primary key, then the scans'
	// for each row found), or the claim (the claims' primary key). A UID is
	// in one of the two at most. The scan is read by a subquery for each
	// row: as a join, a planner that expects few rows may hash the whole
	// scans table, which grows with the tenant.
	instanceRowsSQL = `SELECT i.sop_instance_uid, (SELECT study_uid FROM scans WHERE id = i.scan_id),
		(SELECT series_uid FROM scans WHERE id = i.scan_id), i.sha256, i.reception_id, true
		FROM instances AS i WHERE i.sop_instance_uid = ANY($1::text[])
		UNION ALL SELECT sop_instance_uid, study_uid, series_uid, sha256, reception_id, false
		FROM instance_claims WHERE sop_instance_uid = ANY($1::text[])`
	// receivedOfClaimSQL reads the received files a claim names, by the
	// received files' primary key, once for each.
	receivedOfClaimSQL = `SELECT r.sop_instance_uid, r.sha256, r.study_uid, r.series_uid FROM received_files AS r
		JOIN unnest($2::text[], $3::bytea[]) AS c(uid, sha) ON r.sop_instance_uid = c.uid AND r.sha256 = c.sha
		WHERE r.reception_id = $1`
)
