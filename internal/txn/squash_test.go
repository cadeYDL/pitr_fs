package txn

import (
	"context"
	"errors"
	"testing"
)

func TestSquash_DryRunThenAtomicallyCollapsesHistoryAndPins(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	var rootID int64
	var rootHash string
	if err := db.QueryRow(ctx, `
		SELECT id,trim(version_hash) FROM pitr_txn WHERE state='root'`).Scan(
		&rootID, &rootHash); err != nil {
		t.Fatal(err)
	}
	v1 := createSquashVersion(t, mgr, "write-v1")
	v2 := createSquashVersion(t, mgr, "write-v2")
	v3 := createSquashVersion(t, mgr, "write-v3")
	v4 := createSquashVersion(t, mgr, "write-v4")
	end, err := mgr.FindByID(ctx, v3)
	if err != nil {
		t.Fatal(err)
	}

	slices := [][]byte{
		encodedSlice(401, 4),
		encodedSlice(402, 4),
		encodedSlice(403, 4),
	}
	if _, err := db.Exec(ctx,
		"INSERT INTO jfs_chunk(inode,indx,slices) VALUES (7,0,$1)",
		encodedSlice(404, 4)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO jfs_chunk_ref(chunkid,size,refs)
		VALUES (401,4,0),(402,4,0),(403,4,0),(404,4,1)`); err != nil {
		t.Fatal(err)
	}
	ids := []int64{v1, v2, v3}
	for index, id := range ids {
		if _, err := db.Exec(ctx, `
			INSERT INTO pitr_node_history(txn_id,inode,op,snapshot,recorded_at)
			VALUES ($1,7,'U',jsonb_build_object('marker',$2::int),
			        clock_timestamp()+($2::int::text || ' milliseconds')::interval)`,
			id, index+1); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO pitr_chunk_history(txn_id,inode,indx,op,snapshot,recorded_at)
			VALUES ($1,7,0,'U',
			        jsonb_build_object('inode',7,'indx',0,'slices',$3::bytea),
			        clock_timestamp()+($2::int::text || ' milliseconds')::interval)`,
			id, index+1, slices[index]); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx,
			"SELECT pitr_pin_chunk_slices($1,$2)", id, slices[index]); err != nil {
			t.Fatal(err)
		}
	}

	preview, err := mgr.Squash(ctx, SquashOptions{
		BaseHash: rootHash, EndHash: end.VersionHash,
		Message: "发布业务变更", DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.VersionsMerged != 3 || preview.VersionsDeleted != 2 ||
		preview.HistoryBefore != 6 || preview.HistoryAfter != 2 ||
		preview.HistoryDeleted != 4 || preview.Transaction != nil {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	var versionsBefore int64
	if err := db.QueryRow(ctx,
		"SELECT count(*) FROM pitr_txn").Scan(&versionsBefore); err != nil {
		t.Fatal(err)
	}
	if versionsBefore != 5 {
		t.Fatalf("dry-run changed versions: %d", versionsBefore)
	}

	result, err := mgr.Squash(ctx, SquashOptions{
		BaseHash: rootHash, EndHash: end.VersionHash,
		Message: "发布业务变更", ActorUID: 1000, ActorGID: 1001,
		ActorPID: 42, ActorName: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Transaction == nil || result.Transaction.VersionHash != end.VersionHash ||
		result.Transaction.ParentID == nil || *result.Transaction.ParentID != rootID ||
		result.Transaction.Command != "squash" || result.Transaction.PosixOp != "squash" ||
		result.Transaction.ProcessCommand != "" ||
		result.Transaction.Message != "发布业务变更" ||
		result.Transaction.ChangeSummary != "发布业务变更" ||
		result.Transaction.ActorUID != 1000 || result.Transaction.ActorName != "tester" ||
		!result.Transaction.CreatedAt.Equal(result.FirstOperationAt) ||
		result.Transaction.ClosedAt == nil ||
		!result.Transaction.ClosedAt.Equal(result.EndClosedAt) {
		t.Fatalf("unexpected squash transaction: %+v / %+v", result, result.Transaction)
	}

	var versions, nodeRows, chunkRows int64
	if err := db.QueryRow(ctx, `
		SELECT count(*),
		       (SELECT count(*) FROM pitr_node_history),
		       (SELECT count(*) FROM pitr_chunk_history)
		  FROM pitr_txn`).Scan(&versions, &nodeRows, &chunkRows); err != nil {
		t.Fatal(err)
	}
	if versions != 3 || nodeRows != 1 || chunkRows != 1 {
		t.Fatalf("collapsed rows versions=%d node=%d chunk=%d",
			versions, nodeRows, chunkRows)
	}
	var marker int
	if err := db.QueryRow(ctx, `
		SELECT (snapshot->>'marker')::int
		  FROM pitr_node_history WHERE txn_id=$1`, v3).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != 1 {
		t.Fatalf("kept marker=%d, want earliest 1", marker)
	}
	var keptSlice []byte
	if err := db.QueryRow(ctx, `
		SELECT (jsonb_populate_record(NULL::jfs_chunk,snapshot)).slices
		  FROM pitr_chunk_history WHERE txn_id=$1`, v3).Scan(&keptSlice); err != nil {
		t.Fatal(err)
	}
	if string(keptSlice) != string(slices[0]) {
		t.Fatalf("kept slice is not earliest: %x", keptSlice)
	}

	var parent int64
	if err := db.QueryRow(ctx,
		"SELECT parent_id FROM pitr_txn WHERE id=$1", v4).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	if parent != v3 {
		t.Fatalf("descendant parent=%d, want %d", parent, v3)
	}
	rows, err := db.Query(ctx, `
		SELECT chunkid,refs FROM jfs_chunk_ref
		 WHERE chunkid BETWEEN 401 AND 404 ORDER BY chunkid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	wantRefs := []int64{1, 0, 0, 1}
	for index := 0; rows.Next(); index++ {
		var chunkID, refs int64
		if err := rows.Scan(&chunkID, &refs); err != nil {
			t.Fatal(err)
		}
		if index >= len(wantRefs) || refs != wantRefs[index] {
			t.Fatalf("chunk %d refs=%d want=%d", chunkID, refs, wantRefs[index])
		}
	}
	var delayedID int64
	if err := db.QueryRow(ctx,
		"SELECT delayed_id FROM pitr_slice_pin WHERE txn_id=$1", v3).Scan(&delayedID); err != nil {
		t.Fatal(err)
	}
	if delayedID != 8000000000000000000+v3 {
		t.Fatalf("delayed id=%d", delayedID)
	}
}

func TestSquash_RejectsOpenWritesAndInvalidAncestry(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	var rootHash string
	if err := db.QueryRow(ctx,
		"SELECT trim(version_hash) FROM pitr_txn WHERE state='root'").Scan(
		&rootHash); err != nil {
		t.Fatal(err)
	}
	v1 := createSquashVersion(t, mgr, "v1")
	closed, err := mgr.FindByID(ctx, v1)
	if err != nil {
		t.Fatal(err)
	}
	openID, err := mgr.OpenStandaloneVersion(ctx, "/file", "write", VersionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = mgr.Squash(ctx, SquashOptions{
		BaseHash: rootHash, EndHash: closed.VersionHash,
		Message: "x", DryRun: true,
	})
	if !errors.Is(err, ErrOpenWrites) {
		t.Fatalf("open write error=%v", err)
	}
	if err := mgr.AbortAutoVersion(ctx, openID); err != nil {
		t.Fatal(err)
	}
	_, err = mgr.Squash(ctx, SquashOptions{
		BaseHash: closed.VersionHash, EndHash: rootHash,
		Message: "x", DryRun: true,
	})
	if !errors.Is(err, ErrInvalidSquashRange) {
		t.Fatalf("invalid ancestry error=%v", err)
	}
}

func TestSquash_FailureRollsBackWholeRange(t *testing.T) {
	mgr, db := setupManager(t)
	ctx := context.Background()
	var rootHash string
	if err := db.QueryRow(ctx,
		"SELECT trim(version_hash) FROM pitr_txn WHERE state='root'").Scan(
		&rootHash); err != nil {
		t.Fatal(err)
	}
	v1 := createSquashVersion(t, mgr, "v1")
	v2 := createSquashVersion(t, mgr, "v2")
	endBefore, err := mgr.FindByID(ctx, v2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO pitr_chunk_history(txn_id,inode,indx,op,snapshot)
		VALUES ($1,9,0,'U',jsonb_build_object(
		  'inode',9,'indx',0,'slices',decode('01','hex')))`, v1); err != nil {
		t.Fatal(err)
	}

	_, err = mgr.Squash(ctx, SquashOptions{
		BaseHash: rootHash, EndHash: endBefore.VersionHash, Message: "会失败",
	})
	if err == nil {
		t.Fatal("malformed slice snapshot should fail squash")
	}
	var versions, history int64
	if err := db.QueryRow(ctx, `
		SELECT count(*),(SELECT count(*) FROM pitr_chunk_history)
		  FROM pitr_txn`).Scan(&versions, &history); err != nil {
		t.Fatal(err)
	}
	if versions != 3 || history != 1 {
		t.Fatalf("failed squash changed data: versions=%d history=%d", versions, history)
	}
	endAfter, err := mgr.FindByID(ctx, v2)
	if err != nil {
		t.Fatal(err)
	}
	if endAfter.Command == "squash" || endAfter.VersionHash != endBefore.VersionHash {
		t.Fatalf("failed squash changed end: before=%+v after=%+v", endBefore, endAfter)
	}
}

func createSquashVersion(t *testing.T, mgr *Manager, command string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := mgr.OpenStandaloneVersion(
		ctx, "/file", command, VersionMetadata{PosixOp: command})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.CloseStandaloneVersion(ctx, id, command, command); err != nil {
		t.Fatal(err)
	}
	return id
}
