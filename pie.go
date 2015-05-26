package pie

import (
	"errors"
	"fmt"
	"io"
	"net/rpc"
	"os"
	"os/exec"
	"time"
)

var errProcStopTimeout = errors.New("process killed after timeout waiting for process to stop")

// NewProvider returns a plugin provider that will serve RPC over this
// application's Stdin and Stdout.  This method is intended to be run by the
// plugin application.
func NewProvider() Provider {
	return Provider{
		server: rpc.NewServer(),
		rwc:    rwCloser{os.Stdin, os.Stdout},
	}
}

// Provider is a type that will allow you to register types for the API of a
// plugin and then serve those types over RPC.  It encompasses the functionality
// to talk to a master process.
type Provider struct {
	server *rpc.Server
	rwc    io.ReadWriteCloser
}

// Serve starts the plugin's RPC server, serving via gob encoding.  This call
// will block until the client hangs up.
func (p Provider) Serve() {
	p.server.ServeConn(p.rwc)
}

// ServeCodec starts the plugin's RPC server, serving via the encoding returned by f.
// This call will block until the client hangs up.
func (p Provider) ServeCodec(f func(io.ReadWriteCloser) rpc.ServerCodec) {
	p.server.ServeCodec(f(p.rwc))
}

// Register publishes in the provider the set of methods of the receiver value
// that satisfy the following conditions:
//
//	- exported method
//	- two arguments, both of exported type
//	- the second argument is a pointer
//	- one return value, of type error
//
// It returns an error if the receiver is not an exported type or has no
// suitable methods. It also logs the error using package log. The client
// accesses each method using a string of the form "Type.Method", where Type is
// the receiver's concrete type.
func (p Provider) Register(rcvr interface{}) error {
	return p.server.Register(rcvr)
}

// RegisterName is like Register but uses the provided name for the type
// instead of the receiver's concrete type.
func (p Provider) RegisterName(name string, rcvr interface{}) error {
	return p.server.RegisterName(name, rcvr)
}

// StartProvider start a plugin application at the given path and args, and
// returns an RPC client that communicates with the plugin using gob encoding
// over the plugin's Stdin and Stdout.  The writer passed to output will receive
// output from the plugin's stderr.  Closing the RPC client returned from this
// function will shut down the plugin application.
func StartProvider(output io.Writer, path string, args ...string) (*rpc.Client, error) {
	rwc, err := start(makeCommand(output, path, args))
	if err != nil {
		return nil, err
	}
	return rpc.NewClient(rwc), nil
}

// StartProviderCodec starts a plugin application at the given path and args,
// and returns an RPC client that communicates with the plugin using the
// ClientCodec returned by f over the plugin's Stdin and Stdout. The writer
// passed to output will receive output from the plugin's stderr.  Closing the
// RPC client returned from this function will shut down the plugin application.
func StartProviderCodec(
	f func(io.ReadWriteCloser) rpc.ClientCodec,
	output io.Writer,
	path string,
	args ...string,
) (*rpc.Client, error) {
	rwc, err := start(makeCommand(output, path, args))
	if err != nil {
		return nil, err
	}
	return rpc.NewClientWithCodec(f(rwc)), nil
}

// StartConsumer starts a plugin application with the given path and args,
// writing its stderr to output.  The plugin consumes an API this application
// provides.  The function returns the provider for this host application, which
// should be used to register APIs for the plugin to consume.
func StartConsumer(output io.Writer, path string, args ...string) (Provider, error) {
	rwc, err := start(makeCommand(output, path, args))
	if err != nil {
		return Provider{}, err
	}
	return Provider{
		server: rpc.NewServer(),
		rwc:    rwc,
	}, nil
}

// NewConsumer returns an rpc.Client that will consume an API from the host
// process over this application's Stdin and Stdout using gob encoding.
func NewConsumer() *rpc.Client {
	return rpc.NewClient(rwCloser{os.Stdin, os.Stdout})
}

// NewConsumerCodec returns an rpc.Client that will consume an API from the host
// process over this application's Stdin and Stdout using the ClientCodec
// returned by f.
func NewConsumerCodec(f func(io.ReadWriteCloser) rpc.ClientCodec) *rpc.Client {
	return rpc.NewClientWithCodec(f(rwCloser{os.Stdin, os.Stdout}))
}

// start runs the plugin and returns a ReadWriteCloser that can be used to
// control the plugin.
func start(cmd commander, proc osProcess) (_ io.ReadWriteCloser, err error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			in.Close()
		}
	}()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			out.Close()
		}
	}()

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return ioPipe{out, in, proc}, nil
}

// makeCommand is a function that just creates an exec.Cmd and the process in
// it. It exists to facilitate testing.
var makeCommand = func(w io.Writer, path string, args []string) (commander, osProcess) {
	cmd := exec.Command(path, args...)
	cmd.Stderr = w
	return cmd, cmd.Process
}

// commander is an interface that is fulfilled by exec.Cmd and makes our testing
// a little easier.
type commander interface {
	StdinPipe() (io.WriteCloser, error)
	StdoutPipe() (io.ReadCloser, error)
	Start() error
}

// osProcess is an interface that is fullfilled by *os.Process and makes our
// testing a little easier.
type osProcess interface {
	Wait() (*os.ProcessState, error)
	Kill() error
	Signal(os.Signal) error
}

// ioPipe simply wraps a ReadCloser, WriteCloser, and a Process, and coordinates
// them so they all close together.
type ioPipe struct {
	io.ReadCloser
	io.WriteCloser
	proc osProcess
}

// Close closes the pipe's WriteCloser, ReadClosers, and process.
func (iop ioPipe) Close() error {
	err := iop.ReadCloser.Close()
	if writeErr := iop.WriteCloser.Close(); writeErr != nil {
		err = writeErr
	}
	if procErr := iop.closeProc(); procErr != nil {
		err = procErr
	}
	return err
}

// procTimeout is the timeout to wait for a process to stop after being
// signalled.  It is adjustable to keep tests fast.
var procTimeout = time.Second

// closeProc sends an interrupt signal to the pipe's process, and if it doesn't
// respond in one second, kills the process.
func (iop ioPipe) closeProc() error {
	result := make(chan error, 1)
	go func() { _, err := iop.proc.Wait(); result <- err }()
	if err := iop.proc.Signal(os.Interrupt); err != nil {
		return err
	}
	select {
	case err := <-result:
		return err
	case <-time.After(procTimeout):
		if err := iop.proc.Kill(); err != nil {
			return fmt.Errorf("error killing process after timeout: %s", err)
		}
		return errProcStopTimeout
	}
}

// rwCloser just merges a ReadCloser and a WriteCloser into a ReadWriteCloser.
type rwCloser struct {
	io.ReadCloser
	io.WriteCloser
}

// Close closes both the ReadCloser and the WriteCloser, returning the last
// error from either.
func (rw rwCloser) Close() error {
	err := rw.ReadCloser.Close()
	if err := rw.WriteCloser.Close(); err != nil {
		return err
	}
	return err
}