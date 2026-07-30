package pitr

import (
	"context"
	"errors"
	"fmt"
	"sync"

	pb "pitr_fs/api/pitrd/v1"
)

type Txn struct {
	client      *Client
	path        string
	txnID       int64
	versionHash string

	mu    sync.Mutex
	state string
}

func (t *Txn) ID() int64 {
	if t == nil {
		return 0
	}
	return t.txnID
}

func (t *Txn) VersionHash() string {
	if t == nil {
		return ""
	}
	return t.versionHash
}

func (t *Txn) Path() string {
	if t == nil {
		return ""
	}
	return t.path
}

func (t *Txn) State() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

func (t *Txn) Commit(ctx context.Context, message string) error {
	if t == nil || t.client == nil {
		return errors.New("nil pitr transaction")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != "active" {
		return fmt.Errorf("%w: state=%s", ErrTxnClosed, t.state)
	}
	response, err := t.client.rpc.Commit(ctx, &pb.CommitRequest{
		TxnId: t.txnID, Message: message,
	})
	if err != nil {
		return fmt.Errorf("commit %s: %w", t.path, err)
	}
	t.state = response.GetTransaction().GetState()
	return nil
}

func (t *Txn) Rollback(ctx context.Context) error {
	if t == nil || t.client == nil {
		return errors.New("nil pitr transaction")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != "active" {
		return fmt.Errorf("%w: state=%s", ErrTxnClosed, t.state)
	}
	response, err := t.client.rpc.Rollback(ctx,
		&pb.RollbackRequest{TxnId: t.txnID})
	if err != nil {
		return fmt.Errorf("rollback %s: %w", t.path, err)
	}
	t.state = response.GetTransaction().GetState()
	return nil
}
