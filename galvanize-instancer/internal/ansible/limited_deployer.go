package ansible

import (
	"context"

	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/pkg/config"
)

// LimitedDeployer runs at most a fixed number of Deploy and Terminate calls
// of the wrapped Deployer at a time; other calls wait for one to finish.
// Without Redis, it bounds the Ansible runs started by team requests and
// expiries, as the worker pool does with Redis.
type LimitedDeployer struct {
	inner Deployer
	slots chan struct{}
}

var _ Deployer = (*LimitedDeployer)(nil)

// NewLimitedDeployer wraps inner so that at most limit calls run at a time
// (at least 1).
func NewLimitedDeployer(inner Deployer, limit int) *LimitedDeployer {
	if limit < 1 {
		limit = 1
	}
	return &LimitedDeployer{inner: inner, slots: make(chan struct{}, limit)}
}

func (d *LimitedDeployer) acquire(ctx context.Context) error {
	select {
	case d.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *LimitedDeployer) release() { <-d.slots }

func (d *LimitedDeployer) Deploy(ctx context.Context, conf *config.Config, chall *challenge.Challenge, teamID string) (string, error) {
	if err := d.acquire(ctx); err != nil {
		return "", err
	}
	defer d.release()
	return d.inner.Deploy(ctx, conf, chall, teamID)
}

func (d *LimitedDeployer) Terminate(ctx context.Context, conf *config.Config, chall *challenge.Challenge, teamID string) error {
	if err := d.acquire(ctx); err != nil {
		return err
	}
	defer d.release()
	return d.inner.Terminate(ctx, conf, chall, teamID)
}
