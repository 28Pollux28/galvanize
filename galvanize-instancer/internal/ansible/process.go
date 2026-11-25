package ansible

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/apenella/go-ansible/v2/pkg/execute"
	goexec "github.com/apenella/go-ansible/v2/pkg/execute/exec"
	"go.uber.org/zap"
)

// Stopping ansible-playbook (utility playbooks: builds and image actions).
//
// go-ansible starts ansible-playbook with exec.CommandContext and reads its
// output until EOF before waiting for it. Ansible runs each task in a worker
// process that leads its own session, so neither a SIGKILL to
// ansible-playbook nor a signal to its process group reaches the workers:
// they, and the ssh connections they hold, went on after a cancellation.
// ansible-playbook stops its workers itself when it gets SIGTERM, so a
// cancelled context sends it SIGTERM. If the run has not ended after
// stopGrace (ansible-playbook ignores SIGTERM, or a worker it left keeps
// its output open, and go-ansible waiting), every local process of the run
// is killed: ansible-playbook and what it started, found by the runMarkerVar
// variable in their environment (as stopScript finds a run's commands on
// the hosts; Linux only, elsewhere ansible-playbook alone is killed).
// Execute returns once ansible-playbook has exited and its output is
// closed. Processes of the run still there then get SIGTERM.

// stopGrace is how long a cancelled ansible-playbook has to stop after
// SIGTERM.
var stopGrace = 15 * time.Second

// runMarkerVar marks the local processes of one ansible-playbook run: it is
// set, to a value unique to the run, in the environment of ansible-playbook,
// and the processes it starts inherit it.
const runMarkerVar = "GALVANIZE_ANSIBLE_RUN"

// processExecutable starts ansible-playbook bound to ctx (set by
// cancellableExecutor before each run).
type processExecutable struct {
	ctx    context.Context
	marker string

	mu    sync.Mutex
	kill  *time.Timer // kills the run's processes stopGrace after the cancellation
	ended bool
}

var _ execute.Executabler = (*processExecutable)(nil)

// withProcessExecutable makes def start ansible-playbook with a new
// processExecutable, and marks the run's processes.
func withProcessExecutable(def *execute.DefaultExecute) *processExecutable {
	exe := &processExecutable{marker: generateRandomID() + generateRandomID()}
	def.Exec = exe
	def.AddEnvVar(runMarkerVar, exe.marker)
	return exe
}

func (p *processExecutable) Command(name string, arg ...string) goexec.Cmder {
	return p.CommandContext(context.Background(), name, arg...)
}

// CommandContext ignores the context go-ansible passes, which is never
// cancelled (see cancellableExecutor), and uses p.ctx.
func (p *processExecutable) CommandContext(_ context.Context, name string, arg ...string) goexec.Cmder {
	ctx := p.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, name, arg...)
	cmd.Cancel = func() error {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		p.mu.Lock()
		if !p.ended && p.marker != "" {
			grace, marker := stopGrace, p.marker
			p.kill = time.AfterFunc(grace, func() {
				if n := signalMarked(marker, syscall.SIGKILL); n > 0 {
					zap.S().Warnf("ansible-playbook did not stop %v after SIGTERM: %d process(es) of the run killed", grace, n)
				}
			})
		}
		p.mu.Unlock()
		// Not nil: Wait then reports how ansible-playbook exited (nil or an
		// *exec.ExitError), never ctx.Err(). go-ansible takes any other
		// error of Wait for an *exec.ExitError, and panicked when
		// ansible-playbook exited 0 after the cancellation.
		return os.ErrProcessDone
	}
	// SIGKILL to ansible-playbook where signalMarked finds nothing
	cmd.WaitDelay = stopGrace
	return cmd
}

// end is called when Execute returns: the run's processes still there get
// SIGTERM, and the pending kill is dropped.
func (p *processExecutable) end() {
	p.mu.Lock()
	p.ended = true
	if p.kill != nil {
		p.kill.Stop()
	}
	p.mu.Unlock()
	if p.marker != "" {
		signalMarked(p.marker, syscall.SIGTERM)
	}
}

// signalMarked sends sig to the local processes whose environment has
// runMarkerVar=marker, and returns how many it signalled. It reads /proc:
// elsewhere than on Linux it finds none.
func signalMarked(marker string, sig syscall.Signal) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	want := []byte(runMarkerVar + "=" + marker)
	n := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		env, err := os.ReadFile("/proc/" + e.Name() + "/environ")
		if err != nil {
			continue
		}
		for _, v := range bytes.Split(env, []byte{0}) {
			if bytes.Equal(v, want) {
				if syscall.Kill(pid, sig) == nil {
					n++
				}
				break
			}
		}
	}
	return n
}

// cancellableExecutor runs a go-ansible executor built with exe. go-ansible
// stops reading the output and returns as soon as the context it gets is
// cancelled, without waiting for the process: it gets a context that is
// never cancelled, and the process, started by exe with the real one, is
// stopped as described above. Execute returns when the process has exited.
type cancellableExecutor struct {
	inner execute.Executor
	exe   *processExecutable
}

func (c cancellableExecutor) Execute(ctx context.Context) error {
	c.exe.ctx = ctx
	err := c.run(ctx)
	c.exe.end()
	return err
}

// run turns a panic of go-ansible into an error, as safeCall does for the
// operations: the runs happen in goroutines of their own, where a panic
// would stop Galvanize.
func (c cancellableExecutor) run(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			zap.S().Errorf("ansible-playbook run panicked: %v", r)
			err = fmt.Errorf("internal error running ansible-playbook: %v", r)
		}
	}()
	return c.inner.Execute(context.WithoutCancel(ctx))
}
