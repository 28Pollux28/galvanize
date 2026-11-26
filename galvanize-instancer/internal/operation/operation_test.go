package operation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func waitDone(t *testing.T, op *Operation) Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if snap := op.Snapshot(0); snap.Status != StatusRunning {
			return snap
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("operation did not finish")
	return Snapshot{}
}

func TestStart_SuccessKeepsLogAndResult(t *testing.T) {
	m := NewManager()
	op, err := m.Start("build", "repo", func(ctx context.Context, log Logger) (map[string]interface{}, error) {
		log.Infof("one %d", 1)
		log.Successf("two\nthree")
		return map[string]interface{}{"built": 2}, nil
	})
	require.NoError(t, err)
	snap := waitDone(t, op)

	assert.Equal(t, StatusSuccess, snap.Status)
	assert.Equal(t, 2, snap.Result["built"])
	require.Len(t, snap.Lines, 3, "multi-line messages are split")
	assert.Equal(t, "one 1", snap.Lines[0].Message)
	assert.Equal(t, LevelSuccess, snap.Lines[2].Level)
	assert.NotNil(t, snap.FinishedAt)
}

func TestStart_ErrorLinesMakeAWarning(t *testing.T) {
	m := NewManager()
	op, _ := m.Start("build", "x", func(ctx context.Context, log Logger) (map[string]interface{}, error) {
		log.Errorf("one build failed")
		return nil, nil
	})
	assert.Equal(t, StatusWarning, waitDone(t, op).Status)
}

func TestStart_ReturnedErrorFails(t *testing.T) {
	m := NewManager()
	op, _ := m.Start("clone", "x", func(ctx context.Context, log Logger) (map[string]interface{}, error) {
		return nil, errors.New("git clone failed")
	})
	snap := waitDone(t, op)
	assert.Equal(t, StatusFailed, snap.Status)
	assert.Equal(t, "git clone failed", snap.Lines[len(snap.Lines)-1].Message)
}

func TestStart_PanicFailsTheOperationOnly(t *testing.T) {
	m := NewManager()
	op, _ := m.Start("clone", "x", func(ctx context.Context, log Logger) (map[string]interface{}, error) {
		panic("boom")
	})
	assert.Equal(t, StatusFailed, waitDone(t, op).Status)
	// The manager is free again
	op2, err := m.Start("clone", "y", func(ctx context.Context, log Logger) (map[string]interface{}, error) { return nil, nil })
	require.NoError(t, err)
	waitDone(t, op2)
}

func TestStart_OneAtATime(t *testing.T) {
	m := NewManager()
	release := make(chan struct{})
	first, err := m.Start("build", "a", func(ctx context.Context, log Logger) (map[string]interface{}, error) {
		<-release
		return nil, nil
	})
	require.NoError(t, err)

	running, err := m.Start("pull", "b", func(ctx context.Context, log Logger) (map[string]interface{}, error) { return nil, nil })
	assert.ErrorIs(t, err, ErrBusy)
	assert.Equal(t, first.ID, running.ID, "the running operation is returned")

	close(release)
	waitDone(t, first)
	_, err = m.Start("pull", "b", func(ctx context.Context, log Logger) (map[string]interface{}, error) { return nil, nil })
	assert.NoError(t, err)
}

func TestSnapshot_Offset(t *testing.T) {
	m := NewManager()
	op, _ := m.Start("build", "x", func(ctx context.Context, log Logger) (map[string]interface{}, error) {
		for i := 0; i < 5; i++ {
			log.Infof("line %d", i)
		}
		return nil, nil
	})
	waitDone(t, op)
	snap := op.Snapshot(3)
	assert.Equal(t, 5, snap.LineCount)
	require.Len(t, snap.Lines, 2)
	assert.Equal(t, "line 3", snap.Lines[0].Message)
	assert.Empty(t, op.Snapshot(-1).Lines, "a negative offset returns no lines")
	assert.Empty(t, op.Snapshot(99).Lines)
}

func TestCancel_CancelsTheContext(t *testing.T) {
	m := NewManager()
	op, _ := m.Start("build", "x", func(ctx context.Context, log Logger) (map[string]interface{}, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	found, running := m.Cancel(op.ID)
	assert.True(t, found)
	assert.True(t, running)
	assert.Equal(t, StatusCancelled, waitDone(t, op).Status)
	found, _ = m.Cancel("nope")
	assert.False(t, found)
	// A finished operation is left as it is
	found, running = m.Cancel(op.ID)
	assert.True(t, found)
	assert.False(t, running)

	// Cancelled even when the body ignores the error it got
	op, _ = m.Start("build", "y", func(ctx context.Context, log Logger) (map[string]interface{}, error) {
		<-ctx.Done()
		return map[string]interface{}{"built": []string{}}, nil
	})
	m.Cancel(op.ID)
	assert.Equal(t, StatusCancelled, waitDone(t, op).Status)
}

func TestList_MostRecentFirstAndBounded(t *testing.T) {
	m := NewManager()
	var last *Operation
	for i := 0; i < maxKept+5; i++ {
		op, err := m.Start("build", "x", func(ctx context.Context, log Logger) (map[string]interface{}, error) { return nil, nil })
		require.NoError(t, err)
		waitDone(t, op)
		last = op
	}
	list := m.List()
	assert.Len(t, list, maxKept)
	assert.Equal(t, last.ID, list[0].ID)
	_, found := m.Get(last.ID)
	assert.True(t, found)
}
