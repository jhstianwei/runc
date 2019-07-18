// +build linux

package libcontainer

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Sirupsen/logrus"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/utils"
)

func newStateTransitionError(from, to containerState) error {
	return &stateTransitionError{
		From: from.status().String(),
		To:   to.status().String(),
	}
}

// stateTransitionError is returned when an invalid state transition happens from one
// state to another.
type stateTransitionError struct {
	From string
	To   string
}

func (s *stateTransitionError) Error() string {
	return fmt.Sprintf("invalid state transition from %s to %s", s.From, s.To)
}

type containerState interface {
	transition(containerState) error
	destroy() error
	status() Status
}

func destroy(c *linuxContainer) error {
	if !c.config.Namespaces.Contains(configs.NEWPID) {
		if err := killCgroupProcesses(c.cgroupManager); err != nil {
			logrus.Warn(err)
		}
	}

	s, err := c.Status()
	f, err := os.OpenFile("/tmp/status.txt", os.O_WRONLY|os.O_APPEND, 0666)
	defer f.Close()
	f.WriteString("destroy start to init logs....")
	f.WriteString(fmt.Sprintf("get status: %#v", s))
	f.WriteString("destroy end to init logs....")
	err = c.cgroupManager.Destroy()
	if rerr := os.RemoveAll(c.root); err == nil {
		err = rerr
	}
	c.initProcess = nil
	if herr := runPoststopHooks(c); err == nil {
		err = herr
	}
	c.state = &stoppedState{c: c}
	return err
}

func runPoststopHooks(c *linuxContainer) error {
	if c.config.Hooks != nil {
		s := configs.HookState{
			Version:    c.config.Version,
			ID:         c.id,
			Root:       c.config.Rootfs,
			BundlePath: utils.SearchLabels(c.config.Labels, "bundle"),
		}
		for _, hook := range c.config.Hooks.Poststop {
			if err := hook.Run(s); err != nil {
				return err
			}
		}
	}
	return nil
}

// stoppedState represents a container is a stopped/destroyed state.
type stoppedState struct {
	c *linuxContainer
}

func (b *stoppedState) status() Status {
	return Stopped
}

func (b *stoppedState) transition(s containerState) error {
	switch s.(type) {
	case *runningState:
		b.c.state = s
		return nil
	case *restoredState:
		b.c.state = s
		return nil
	case *stoppedState:
		return nil
	}
	return newStateTransitionError(b, s)
}

func (b *stoppedState) destroy() error {
	return destroy(b.c)
}

// runningState represents a container that is currently running.
type runningState struct {
	c *linuxContainer
}

func (r *runningState) status() Status {
	return Running
}

func (r *runningState) transition(s containerState) error {
	switch s.(type) {
	case *stoppedState:
		t, err := r.c.runType()
		if err != nil {
			return err
		}
		if t == Running {
			return newGenericError(fmt.Errorf("container still running"), ContainerNotStopped)
		}
		r.c.state = s
		return nil
	case *pausedState:
		r.c.state = s
		return nil
	case *runningState:
		return nil
	}
	return newStateTransitionError(r, s)
}

func (r *runningState) destroy() error {
	t, err := r.c.runType()
	if err != nil {
		return err
	}
	if t == Running {
		return newGenericError(fmt.Errorf("container is not destroyed"), ContainerNotStopped)
	}
	return destroy(r.c)
}

type createdState struct {
	c *linuxContainer
}

func (i *createdState) status() Status {
	return Created
}

func (i *createdState) transition(s containerState) error {
	switch s.(type) {
	case *runningState, *pausedState, *stoppedState:
		i.c.state = s
		return nil
	case *createdState:
		return nil
	}
	return newStateTransitionError(i, s)
}

func (i *createdState) destroy() error {
	i.c.initProcess.signal(syscall.SIGKILL)
	return destroy(i.c)
}

// pausedState represents a container that is currently pause.  It cannot be destroyed in a
// paused state and must transition back to running first.
type pausedState struct {
	c *linuxContainer
}

func (p *pausedState) status() Status {
	return Paused
}

func (p *pausedState) transition(s containerState) error {
	switch s.(type) {
	case *runningState, *stoppedState:
		p.c.state = s
		return nil
	case *pausedState:
		return nil
	}
	return newStateTransitionError(p, s)
}

func (p *pausedState) destroy() error {
	t, err := p.c.runType()
	if err != nil {
		return err
	}
	if t != Running && t != Created {
		if err := p.c.cgroupManager.Freeze(configs.Thawed); err != nil {
			return err
		}
		return destroy(p.c)
	}
	return newGenericError(fmt.Errorf("container is paused"), ContainerPaused)
}

// restoredState is the same as the running state but also has accociated checkpoint
// information that maybe need destroyed when the container is stopped and destroy is called.
type restoredState struct {
	imageDir string
	c        *linuxContainer
}

func (r *restoredState) status() Status {
	return Running
}

func (r *restoredState) transition(s containerState) error {
	switch s.(type) {
	case *stoppedState:
		return nil
	case *runningState:
		return nil
	}
	return newStateTransitionError(r, s)
}

func (r *restoredState) destroy() error {
	if _, err := os.Stat(filepath.Join(r.c.root, "checkpoint")); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	}
	return destroy(r.c)
}

// loadedState is used whenever a container is restored, loaded, or setting additional
// processes inside and it should not be destroyed when it is exiting.
type loadedState struct {
	c *linuxContainer
	s Status
}

func (n *loadedState) status() Status {
	return n.s
}

func (n *loadedState) transition(s containerState) error {
	n.c.state = s
	return nil
}

func (n *loadedState) destroy() error {
	if err := n.c.refreshState(); err != nil {
		return err
	}
	return n.c.state.destroy()
}
