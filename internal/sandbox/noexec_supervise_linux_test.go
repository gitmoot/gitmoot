//go:build linux

package sandbox

import (
	"testing"

	"golang.org/x/sys/unix"
)

// scriptedListener replays a fixed sequence of poll/recv results and records
// every response sent; it fails the test if the loop reads past the script.
type scriptedListener struct {
	t      *testing.T
	events []scriptedEvent
	sent   []seccompNotifResp
	closed bool
}

type scriptedEvent struct {
	revents int16
	waitErr error
	req     seccompNotif
	recvErr error
	sendErr error
}

func (l *scriptedListener) next() *scriptedEvent {
	if len(l.events) == 0 {
		l.t.Fatal("supervisor kept polling after the script ended (it ignored POLLHUP)")
	}
	return &l.events[0]
}

func (l *scriptedListener) wait() (int16, error) {
	ev := l.next()
	if ev.waitErr != nil || ev.revents&unix.POLLIN == 0 {
		l.events = l.events[1:]
	}
	return ev.revents, ev.waitErr
}

func (l *scriptedListener) recv(req *seccompNotif) error {
	ev := l.next()
	*req = ev.req
	if ev.recvErr != nil {
		l.events = l.events[1:]
	}
	return ev.recvErr
}

func (l *scriptedListener) send(resp *seccompNotifResp) error {
	ev := l.next()
	l.events = l.events[1:]
	l.sent = append(l.sent, *resp)
	return ev.sendErr
}

func (l *scriptedListener) close() { l.closed = true }

// TestExecSupervisorSurvivesTransientErrors: #2615. RECV failing with ENOENT
// (the target was signalled mid-notification) or EINTR, SEND failing with
// ENOENT, and a bare POLLERR (notify_lock interrupted by a signal) must not
// end the supervisor; only POLLHUP does. Every exec after bootstrap is still
// answered with EACCES.
func TestExecSupervisorSurvivesTransientErrors(t *testing.T) {
	arch := seccompArches["amd64"]
	const child = 100
	exec := func(id uint64, pid uint32) seccompNotif {
		return seccompNotif{ID: id, Pid: pid, Nr: int32(arch.execve)}
	}
	l := &scriptedListener{t: t, events: []scriptedEvent{
		{revents: unix.POLLIN, req: exec(1, child)},                     // bootstrap
		{revents: unix.POLLIN, recvErr: unix.ENOENT},                    // target preempted
		{waitErr: unix.EINTR},                                           // poll interrupted
		{revents: unix.POLLIN, recvErr: unix.EINTR},                     // recv interrupted
		{revents: unix.POLLIN, req: exec(2, 101), sendErr: unix.ENOENT}, // target gone
		{revents: unix.POLLIN, req: exec(3, child)},                     // later exec
		{revents: unix.POLLERR},                                         // poll lock interrupted
		{revents: unix.POLLIN, req: exec(4, 102)},                       // exec after POLLERR
		{revents: unix.POLLHUP},                                         // no task left
	}}
	runExecSupervisor(l, child, arch)
	if !l.closed {
		t.Fatal("listener not closed on exit")
	}
	if len(l.events) != 0 {
		t.Fatalf("supervisor stopped early with %d scripted events unserved", len(l.events))
	}
	want := []seccompNotifResp{
		{ID: 1, Flags: unix.SECCOMP_USER_NOTIF_FLAG_CONTINUE},
		{ID: 2, Error: -int32(unix.EACCES)},
		{ID: 3, Error: -int32(unix.EACCES)},
		{ID: 4, Error: -int32(unix.EACCES)},
	}
	if len(l.sent) != len(want) {
		t.Fatalf("sent %+v, want %+v", l.sent, want)
	}
	for i := range want {
		if l.sent[i] != want[i] {
			t.Fatalf("response %d = %+v, want %+v", i, l.sent[i], want[i])
		}
	}
}

// TestExecSupervisorStopsOnUnrecoverableError: a dead listener ends the loop.
func TestExecSupervisorStopsOnUnrecoverableError(t *testing.T) {
	l := &scriptedListener{t: t, events: []scriptedEvent{{waitErr: unix.EBADF}}}
	runExecSupervisor(l, 1, seccompArches["amd64"])
	if !l.closed || len(l.events) != 0 {
		t.Fatalf("closed=%v remaining=%d, want closed with script consumed", l.closed, len(l.events))
	}
}
