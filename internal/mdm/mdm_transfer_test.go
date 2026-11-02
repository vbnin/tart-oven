package mdm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type fakeRemoteProfileFS struct {
	files      map[string][]byte
	readResult []byte
	writeErr   error
	readErr    error
	closeErr   error
	wrotePath  string
	wroteMode  fs.FileMode
	closed     bool
}

func (f *fakeRemoteProfileFS) WriteFile(path string, data []byte, perm fs.FileMode) error {
	f.wrotePath = path
	f.wroteMode = perm
	if f.writeErr != nil {
		return f.writeErr
	}
	f.files[path] = bytes.Clone(data)
	return nil
}

func (f *fakeRemoteProfileFS) ReadFile(path string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	if f.readResult != nil {
		return bytes.Clone(f.readResult), nil
	}
	return bytes.Clone(f.files[path]), nil
}

func (f *fakeRemoteProfileFS) Close() error {
	f.closed = true
	return f.closeErr
}

type fakeRemoteProfileDialer struct {
	remote remoteProfileFS
	err    error
}

func (d fakeRemoteProfileDialer) Dial(context.Context, TransferTarget) (remoteProfileFS, error) {
	if d.err != nil {
		return nil, d.err
	}
	return d.remote, nil
}

func TestSFTPProfileCopierCopiesAndVerifiesProfile(t *testing.T) {
	remote := &fakeRemoteProfileFS{files: map[string][]byte{}}
	copier := &sftpProfileCopier{dialer: fakeRemoteProfileDialer{remote: remote}}
	profile := []byte("profile uuid-123")

	err := copier.CopyAndVerify(context.Background(), TransferTarget{
		Address: "192.0.2.10:22", Username: "admin", Password: "admin", Timeout: time.Second,
	}, profile, "uuid-123")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(remote.files[ProfileRemotePath], profile) {
		t.Fatal("wrong remote profile")
	}
	if remote.wrotePath != ProfileRemotePath {
		t.Fatalf("wrote path %q", remote.wrotePath)
	}
	if remote.wroteMode != 0o600 {
		t.Fatalf("wrote mode %04o", remote.wroteMode)
	}
	if !remote.closed {
		t.Fatal("remote filesystem was not closed")
	}
}

func TestSFTPProfileCopierReportsFailureStageAndClosesRemote(t *testing.T) {
	tests := []struct {
		name       string
		dialErr    error
		remote     *fakeRemoteProfileFS
		profile    []byte
		uuid       string
		wantStage  Stage
		wantClosed bool
	}{
		{
			name:      "dial failure",
			dialErr:   errors.New("dial failed"),
			remote:    &fakeRemoteProfileFS{files: map[string][]byte{}},
			profile:   []byte("profile uuid-123"),
			uuid:      "uuid-123",
			wantStage: StageAuthentication,
		},
		{
			name:       "write failure",
			remote:     &fakeRemoteProfileFS{files: map[string][]byte{}, writeErr: errors.New("write failed")},
			profile:    []byte("profile uuid-123"),
			uuid:       "uuid-123",
			wantStage:  StageSFTP,
			wantClosed: true,
		},
		{
			name:       "read failure",
			remote:     &fakeRemoteProfileFS{files: map[string][]byte{}, readErr: errors.New("read failed")},
			profile:    []byte("profile uuid-123"),
			uuid:       "uuid-123",
			wantStage:  StageVerification,
			wantClosed: true,
		},
		{
			name:       "byte mismatch",
			remote:     &fakeRemoteProfileFS{files: map[string][]byte{}, readResult: []byte("different uuid-123")},
			profile:    []byte("profile uuid-123"),
			uuid:       "uuid-123",
			wantStage:  StageVerification,
			wantClosed: true,
		},
		{
			name:       "missing UUID",
			remote:     &fakeRemoteProfileFS{files: map[string][]byte{}},
			profile:    []byte("profile without expected identifier"),
			uuid:       "uuid-123",
			wantStage:  StageVerification,
			wantClosed: true,
		},
		{
			name:       "close failure",
			remote:     &fakeRemoteProfileFS{files: map[string][]byte{}, closeErr: errors.New("close failed")},
			profile:    []byte("profile uuid-123"),
			uuid:       "uuid-123",
			wantStage:  StageSFTP,
			wantClosed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			copier := &sftpProfileCopier{dialer: fakeRemoteProfileDialer{remote: tt.remote, err: tt.dialErr}}
			err := copier.CopyAndVerify(context.Background(), TransferTarget{
				Address: "192.0.2.10:22", Username: "admin", Password: "secret-password", Timeout: time.Second,
			}, tt.profile, tt.uuid)
			if err == nil {
				t.Fatal("CopyAndVerify returned nil error")
			}
			var stageErr *StageError
			if !errors.As(err, &stageErr) {
				t.Fatalf("error type %T, want *StageError", err)
			}
			if stageErr.Stage != tt.wantStage {
				t.Fatalf("stage = %q, want %q", stageErr.Stage, tt.wantStage)
			}
			if tt.remote.closed != tt.wantClosed {
				t.Fatalf("closed = %v, want %v", tt.remote.closed, tt.wantClosed)
			}
			for _, secret := range []string{"secret-password", string(tt.profile)} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error contains secret %q: %v", secret, err)
				}
			}
		})
	}
}

type shortWriteCloser struct {
	bytes.Buffer
	maxWrite int
	closeErr error
	closed   bool
}

func (w *shortWriteCloser) Write(data []byte) (int, error) {
	if len(data) > w.maxWrite {
		data = data[:w.maxWrite]
	}
	return w.Buffer.Write(data)
}

func (w *shortWriteCloser) Close() error {
	w.closed = true
	return w.closeErr
}

func TestSFTPRemoteProfileFSWritesAllBytesClosesAndSetsMode(t *testing.T) {
	writer := &shortWriteCloser{maxWrite: 3}
	var openedPath string
	var openedFlags int
	var chmodPath string
	var chmodMode fs.FileMode
	remote := &sftpRemoteProfileFS{
		openFile: func(path string, flags int) (io.WriteCloser, error) {
			openedPath, openedFlags = path, flags
			return writer, nil
		},
		chmod: func(path string, mode fs.FileMode) error {
			chmodPath, chmodMode = path, mode
			return nil
		},
	}
	profile := []byte("profile bytes that require several writes")

	if err := remote.WriteFile(ProfileRemotePath, profile, 0o600); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(writer.Bytes(), profile) {
		t.Fatalf("written bytes = %q", writer.Bytes())
	}
	if !writer.closed {
		t.Fatal("remote file was not closed")
	}
	if openedPath != ProfileRemotePath || chmodPath != ProfileRemotePath {
		t.Fatalf("open path = %q, chmod path = %q", openedPath, chmodPath)
	}
	wantFlags := os.O_CREATE | os.O_TRUNC | os.O_WRONLY
	if openedFlags != wantFlags {
		t.Fatalf("open flags = %d, want %d", openedFlags, wantFlags)
	}
	if chmodMode != 0o600 {
		t.Fatalf("chmod mode = %04o", chmodMode)
	}
}

func TestSFTPRemoteProfileFSReturnsFileCloseError(t *testing.T) {
	closeErr := errors.New("close remote file")
	writer := &shortWriteCloser{maxWrite: 100, closeErr: closeErr}
	remote := &sftpRemoteProfileFS{
		openFile: func(string, int) (io.WriteCloser, error) { return writer, nil },
		chmod:    func(string, fs.FileMode) error { return nil },
	}

	err := remote.WriteFile(ProfileRemotePath, []byte("profile"), 0o600)
	if !errors.Is(err, closeErr) {
		t.Fatalf("error = %v, want close error", err)
	}
}

func TestSFTPRemoteProfileFSReadsExactBytes(t *testing.T) {
	want := []byte{0x00, 0x01, 0xfe, 0xff}
	remote := &sftpRemoteProfileFS{
		readFile: func(path string) ([]byte, error) {
			if path != ProfileRemotePath {
				t.Fatalf("read path = %q", path)
			}
			return bytes.Clone(want), nil
		},
	}

	got, err := remote.ReadFile(ProfileRemotePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("read bytes = %v, want %v", got, want)
	}
}

type trackedNetConn struct {
	net.Conn
	closeOnce sync.Once
	closed    chan struct{}
	mu        sync.Mutex
	deadline  time.Time
}

func newTrackedNetConn(conn net.Conn) *trackedNetConn {
	return &trackedNetConn{Conn: conn, closed: make(chan struct{})}
}

func (c *trackedNetConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func (c *trackedNetConn) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.deadline = deadline
	c.mu.Unlock()
	return c.Conn.SetDeadline(deadline)
}

func (c *trackedNetConn) hasDeadline() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.deadline.IsZero()
}

func (c *trackedNetConn) deadlineValue() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline
}

func TestSSHSFTPProfileDialerCancelsHandshakeAndClosesConnection(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	tracked := newTrackedNetConn(clientSide)
	t.Cleanup(func() { serverSide.Close() })
	go io.Copy(io.Discard, serverSide)
	started := make(chan struct{})
	dialer := sshSFTPProfileDialer{
		dialContext: func(context.Context, string, string) (net.Conn, error) {
			return tracked, nil
		},
		newClientConn: func(conn net.Conn, address string, config *ssh.ClientConfig) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
			close(started)
			return ssh.NewClientConn(conn, address, config)
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := dialer.Dial(ctx, TransferTarget{
			Address: "192.0.2.10:22", Username: "admin", Password: "admin", Timeout: time.Second,
		})
		result <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("SSH handshake did not start")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Dial returned nil error")
		}
	case <-time.After(time.Second):
		t.Fatal("Dial did not stop after cancellation")
	}
	select {
	case <-tracked.closed:
	default:
		t.Fatal("network connection was not closed")
	}
	if !tracked.hasDeadline() {
		t.Fatal("network connection did not receive a deadline")
	}
}

type fakeSSHConn struct {
	closed bool
}

func (c *fakeSSHConn) User() string          { return "admin" }
func (c *fakeSSHConn) SessionID() []byte     { return []byte("session") }
func (c *fakeSSHConn) ClientVersion() []byte { return []byte("client") }
func (c *fakeSSHConn) ServerVersion() []byte { return []byte("server") }
func (c *fakeSSHConn) RemoteAddr() net.Addr  { return fakeNetAddr("remote") }
func (c *fakeSSHConn) LocalAddr() net.Addr   { return fakeNetAddr("local") }
func (c *fakeSSHConn) Close() error          { c.closed = true; return nil }
func (c *fakeSSHConn) Wait() error           { return nil }
func (c *fakeSSHConn) SendRequest(string, bool, []byte) (bool, []byte, error) {
	return false, nil, nil
}
func (c *fakeSSHConn) OpenChannel(string, []byte) (ssh.Channel, <-chan *ssh.Request, error) {
	return nil, nil, errors.New("not implemented")
}

type fakeNetAddr string

func (a fakeNetAddr) Network() string { return string(a) }
func (a fakeNetAddr) String() string  { return string(a) }

func TestSSHSFTPProfileDialerClosesPartialResourcesWhenSFTPSetupFails(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	tracked := newTrackedNetConn(clientSide)
	t.Cleanup(func() { serverSide.Close() })
	sshConn := &fakeSSHConn{}
	sftpErr := errors.New("start sftp failed")
	var gotConfig *ssh.ClientConfig
	dialer := sshSFTPProfileDialer{
		dialContext: func(context.Context, string, string) (net.Conn, error) {
			return tracked, nil
		},
		newClientConn: func(_ net.Conn, _ string, config *ssh.ClientConfig) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
			gotConfig = config
			newChannels := make(chan ssh.NewChannel)
			requests := make(chan *ssh.Request)
			close(newChannels)
			close(requests)
			return sshConn, newChannels, requests, nil
		},
		newSFTPClient: func(*ssh.Client) (*sftp.Client, error) {
			return nil, sftpErr
		},
	}

	_, err := dialer.Dial(context.Background(), TransferTarget{
		Address: "192.0.2.10:22", Username: "admin", Password: "secret", Timeout: time.Second,
	})
	if !errors.Is(err, sftpErr) {
		t.Fatalf("error = %v, want SFTP setup error", err)
	}
	if !sshConn.closed {
		t.Fatal("SSH client was not closed")
	}
	select {
	case <-tracked.closed:
	default:
		t.Fatal("network connection was not closed")
	}
	if gotConfig == nil || gotConfig.User != "admin" || gotConfig.Timeout != time.Second || len(gotConfig.Auth) != 1 {
		t.Fatalf("SSH config = %#v", gotConfig)
	}
}

func TestSFTPProfileCopierReportsProductionSFTPSetupFailureAsSFTP(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	tracked := newTrackedNetConn(clientSide)
	t.Cleanup(func() { serverSide.Close() })
	sshConn := &fakeSSHConn{}
	sftpErr := errors.New("SFTP subsystem rejected")
	copier := &sftpProfileCopier{dialer: &sshSFTPProfileDialer{
		dialContext: func(context.Context, string, string) (net.Conn, error) {
			return tracked, nil
		},
		newClientConn: func(net.Conn, string, *ssh.ClientConfig) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
			newChannels := make(chan ssh.NewChannel)
			requests := make(chan *ssh.Request)
			close(newChannels)
			close(requests)
			return sshConn, newChannels, requests, nil
		},
		newSFTPClient: func(*ssh.Client) (*sftp.Client, error) {
			return nil, sftpErr
		},
	}}

	err := copier.CopyAndVerify(context.Background(), TransferTarget{
		Address: "192.0.2.10:22", Username: "admin", Password: "secret", Timeout: time.Second,
	}, []byte("profile uuid-123"), "uuid-123")
	var stageErr *StageError
	if !errors.As(err, &stageErr) {
		t.Fatalf("error type %T, want *StageError", err)
	}
	if stageErr.Stage != StageSFTP {
		t.Fatalf("stage = %q, want %q", stageErr.Stage, StageSFTP)
	}
	if !errors.Is(err, sftpErr) {
		t.Fatalf("error = %v, want SFTP setup cause", err)
	}
}

func TestSSHSFTPProfileDialerUsesOneDeadlineForDialAndTransfer(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	tracked := newTrackedNetConn(clientSide)
	t.Cleanup(func() { serverSide.Close() })
	const timeout = 250 * time.Millisecond
	var dialDeadline time.Time
	handshakeErr := errors.New("stop after dial")
	dialer := sshSFTPProfileDialer{
		dialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var ok bool
			dialDeadline, ok = ctx.Deadline()
			if !ok {
				t.Fatal("DialContext did not receive a deadline")
			}
			time.Sleep(25 * time.Millisecond)
			return tracked, nil
		},
		newClientConn: func(net.Conn, string, *ssh.ClientConfig) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
			return nil, nil, nil, handshakeErr
		},
	}

	_, err := dialer.Dial(context.Background(), TransferTarget{
		Address: "192.0.2.10:22", Username: "admin", Password: "secret", Timeout: timeout,
	})
	if !errors.Is(err, handshakeErr) {
		t.Fatalf("error = %v, want handshake error", err)
	}
	connectionDeadline := tracked.deadlineValue()
	if !connectionDeadline.Equal(dialDeadline) {
		t.Fatalf("connection deadline = %v, DialContext deadline = %v", connectionDeadline, dialDeadline)
	}
	if remaining := time.Until(connectionDeadline); remaining >= timeout-10*time.Millisecond {
		t.Fatalf("deadline restarted after dialing; remaining = %v, timeout = %v", remaining, timeout)
	}
}
