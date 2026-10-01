package ansible

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slowDeployer records how many calls run at the same time
type slowDeployer struct {
	running, peak atomic.Int32
	calls         atomic.Int32
}

func (d *slowDeployer) run() {
	d.calls.Add(1)
	n := d.running.Add(1)
	for {
		p := d.peak.Load()
		if n <= p || d.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	d.running.Add(-1)
}

func (d *slowDeployer) Deploy(context.Context, *config.Config, *challenge.Challenge, string) (string, error) {
	d.run()
	return "ok", nil
}

func (d *slowDeployer) Terminate(context.Context, *config.Config, *challenge.Challenge, string) error {
	d.run()
	return nil
}

func TestLimitedDeployer_BoundsConcurrentCalls(t *testing.T) {
	inner := &slowDeployer{}
	d := NewLimitedDeployer(inner, 3)
	chall := &challenge.Challenge{Name: "login", Category: "web"}

	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				_, err := d.Deploy(context.Background(), &config.Config{}, chall, "team")
				assert.NoError(t, err)
			} else {
				assert.NoError(t, d.Terminate(context.Background(), &config.Config{}, chall, "team"))
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(12), inner.calls.Load(), "every call runs")
	assert.Equal(t, int32(3), inner.peak.Load(), "deploys and terminations share the 3 slots")
}

func TestLimitedDeployer_WaitingCallHonorsContext(t *testing.T) {
	inner := &blockingDeployer{release: make(chan struct{})}
	d := NewLimitedDeployer(inner, 1)
	chall := &challenge.Challenge{Name: "login", Category: "web"}

	go func() { _, _ = d.Deploy(context.Background(), &config.Config{}, chall, "a") }()
	require.Eventually(t, func() bool { return inner.started.Load() == 1 }, time.Second, time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := d.Deploy(ctx, &config.Config{}, chall, "b")
	assert.ErrorIs(t, err, context.DeadlineExceeded, "gives up waiting for a slot")
	assert.Equal(t, int32(1), inner.started.Load(), "and never ran")
	close(inner.release)
}

func TestLimitedDeployer_WaitingTerminateHonorsContext(t *testing.T) {
	inner := &blockingDeployer{release: make(chan struct{})}
	d := NewLimitedDeployer(inner, 1)
	chall := &challenge.Challenge{Name: "login", Category: "web"}

	go func() { _, _ = d.Deploy(context.Background(), &config.Config{}, chall, "a") }()
	require.Eventually(t, func() bool { return inner.started.Load() == 1 }, time.Second, time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, d.Terminate(ctx, &config.Config{}, chall, "b"), context.Canceled)
	close(inner.release)
}

func TestNewLimitedDeployer_AtLeastOneSlot(t *testing.T) {
	assert.Equal(t, 1, NewLimitedDeployer(&slowDeployer{}, 0).Limit())
	assert.Equal(t, 4, NewLimitedDeployer(&slowDeployer{}, 4).Limit())
}

type blockingDeployer struct {
	started atomic.Int32
	release chan struct{}
}

func (d *blockingDeployer) Deploy(context.Context, *config.Config, *challenge.Challenge, string) (string, error) {
	d.started.Add(1)
	<-d.release
	return "", nil
}

func (d *blockingDeployer) Terminate(context.Context, *config.Config, *challenge.Challenge, string) error {
	return nil
}
