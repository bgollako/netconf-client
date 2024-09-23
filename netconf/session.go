package netconf

import (
	"io"
	"net"

	"golang.org/x/crypto/ssh"
)

type Session interface {
	// Executes the given rpc on the given endpoint and returns the response
	// ExecuteRPC is thread-safe i.e. multiple routines can call it simultaneously
	ExecuteRpc(rpc []byte) ([]byte, error)
	// Returns a channel on which NETCONF notifications from the endpoint are returned.
	// The notifications will be sent in the same order that they are received.
	// This function will not send the rpc to subscribe to notifications.
	// The RPC to subscribe to notifications must be sent using ExecuteRpc prior
	// to calling SubscribeNotifications.
	SubscribeNotifications() (chan<- []byte, error)
	// Closes the NETCONF client and closes the underlying connection to the endpoint.
	Close()
}

// Encapsulates a NETCONF Session to an endpoint
// Created by the NETCONF Client for every new call home it receieves
type session struct {
	// handle to the config
	config *Config
	// handle to the underlying connection
	conn net.Conn
	// hello msg received from the NETCONF server
	hello []byte
	// Write channel that recieves writes
	writeChan chan []byte

	// ssh readers and writers
	writer     io.WriteCloser
	reader     io.Reader
	errReader  io.Reader
	sshSession *ssh.Session
	sshClient  *ssh.Client

	// used to hold leftover bytes from previous read
	leftovers []byte

	// capabilities of the NETCONF server
	capabilities []byte

	// delimiter to be used depending on NETCONF version
	delimiter []byte
}

func (s *session) Write(b []byte) {
	s.writeChan <- b
}

func (s *session) handleConn() error {
	var err error
	if err = s.answerHello(); err != nil {
		return err
	}

	if err = s.establishSsl(); err != nil {
		return err
	}

	if err = s.ackCapabilities(); err != nil {
		return err
	}

	switch s.config.Version {
	case Netconf_Version_1_0_1_1:
		fallthrough
	case Netconf_Version_1_1:
		s.delimiter = []byte(delimiter_1_1)
	default:
		s.delimiter = []byte(delimiter_1_0)
	}

	go s.handleInputs()
	go s.drainErrors()
	return nil
}

// Reads from the output channel are return to response
func (s *session) handleOutput() error {
	var data []byte
	var err error
	for {
		data, err = s.read(s.reader, s.delimiter)
		if err != nil {
			return err
		}
	}
}

// Listens on the write channel and executes them on the
// endpoint.
func (s *session) handleInputs() error {
	for rpc := range s.writeChan {
		if _, err := s.writer.Write(rpc); err != nil {
			return err
		}
	}
	return nil
}

// drain the errors channel
func (s *session) drainErrors() error {
	var err error
	for {
		_, err = s.read(s.errReader, s.delimiter)
		if err != nil {
			return err
		}
	}
}

// establishes a SSL layer over the tcp connection
func (s *session) establishSsl() error {
	username, password := s.config.GetCredentials(s.hello)

	sshConn, cChan, rChan, err := ssh.NewClientConn(s.conn, s.conn.RemoteAddr().String(), &ssh.ClientConfig{
		User:            username,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		BannerCallback:  func(message string) error { return nil },
	})

	sshClient := ssh.NewClient(sshConn, cChan, rChan)
	if err != nil {
		sshClient.Close()
		return err
	}

	session, err := sshClient.NewSession()
	if err != nil {
		sshClient.Close()
		return err
	}

	err = session.RequestSubsystem("netconf")
	if err != nil {
		session.Close()
		sshClient.Close()
		return err
	}

	reader, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		sshClient.Close()
		return err
	}

	writer, err := session.StdinPipe()
	if err != nil {
		session.Close()
		sshClient.Close()
		return err
	}

	errReader, err := session.StderrPipe()
	if err != nil {
		writer.Close()
		session.Close()
		sshClient.Close()
		return err
	}
	s.reader, s.writer, s.errReader, s.sshSession, s.sshClient = reader, writer, errReader, session, sshClient
	return nil
}

// answers reads the inital hello message sent by the NETCONF server
func (s *session) answerHello() error {
	hello := make([]byte, s.config.HelloSize)
	n, err := s.conn.Read(hello)
	if err != nil {
		return err
	}
	hello = hello[:n]
	s.hello = hello
	_, err = s.conn.Write([]byte(ackOk))
	return err
}

// acknowledge capabilities
func (s *session) ackCapabilities() error {
	var err error
	s.capabilities, err = s.read(s.reader, []byte(delimiter_1_0))
	if err != nil {
		return err
	}

	// TODO: Acknowledge according to the given capabilities list
	_, err = s.writer.Write([]byte(capabilities_1_0_1_1))
	if err != nil {
		return err
	}
	return nil
}

func (s *session) read(reader io.Reader, delimiter []byte) ([]byte, error) {
	var d []byte
	var err error
	d, s.leftovers, err = ReadTillDelimiter(reader, delimiter, s.leftovers)
	return d, err
}

// Closes the session
func (s *session) Close() {
	if s.writer != nil {
		s.writer.Close()
	}
	if s.sshSession != nil {
		s.sshSession.Close()
	}
	if s.sshClient != nil {
		s.sshClient.Close()
	}
}
