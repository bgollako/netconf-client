package netconf

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"sync"

	"golang.org/x/crypto/ssh"
)

var re = regexp.MustCompile(`message-id="([0-9]+)"`)

type Session interface {
	// Executes the given rpc on the given endpoint and returns the response
	// ExecuteRPC is thread-safe i.e. multiple routines can call it simultaneously
	// It is the responsibility of the calling function to ensure the uniqueness of message ids
	ExecuteRpc(rpc []byte) ([]byte, error)
	// Returns a channel on which NETCONF notifications from the endpoint are returned.
	// The notifications will be sent in the same order that they are received.
	// This function will not send the rpc to subscribe to notifications.
	// The RPC to subscribe to notifications must be sent using ExecuteRpc prior
	// to calling SubscribeNotifications.
	SubscribeNotifications() <-chan []byte
	// Closes the NETCONF client and closes the underlying connection to the endpoint.
	Close()
}

type Version int

const (
	// NETCONF version 1.0 && 1.1
	Netconf_Version_1_0_1_1 = iota
	// NETCONF version 1.1
	Netconf_Version_1_1
	// NETCONF version 1.0
	Netconf_Version_1_0
)

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

	// channel to send subscriptions over
	subscriptions chan []byte

	// boolean value to check whether notifications are subscribed to
	isSubscribed bool

	// mutex to control access to below map
	m sync.Mutex

	// map to internal channels for given messages
	idmap map[int]chan []byte

	// version of NETCONF to support
	// versions supported are 1.0, 1.1 and both
	// Defaults to to both if unspecified
	version Version
}

func (s *session) SubscribeNotifications() <-chan []byte {
	if !s.isSubscribed {
		s.isSubscribed = true
		s.subscriptions = make(chan []byte)
	}
	return s.subscriptions
}

func (s *session) ExecuteRpc(b []byte) ([]byte, error) {
	id, err := s.msgId(b)
	if err != nil {
		return nil, err
	}
	s.m.Lock()
	if _, ok := s.idmap[id]; ok {
		s.m.Unlock()
		return nil, fmt.Errorf("rpc with id %d already present", id)
	}
	s.idmap[id] = make(chan []byte)
	s.m.Unlock()
	s.writeChan <- b
	d := <-s.idmap[id]
	s.m.Lock()
	delete(s.idmap, id)
	s.m.Unlock()
	return d, nil
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

// handles the incoming connections and answers the call home
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

	go s.handleInput()
	go s.drainErrors()
	go s.handleOutput()
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

		// If it is a notification send it on the notification channel
		if bytes.Contains(data, []byte(tag_notification)) {
			if s.isSubscribed {
				s.subscriptions <- s.sanitize(data)
			}
		} else if bytes.Contains(data, []byte(tag_rpc_error)) || bytes.Contains(data, []byte(tag_rpc_reply)) {
			id, err := s.msgId(data)
			if err == nil {
				s.idmap[id] <- s.sanitize(data)
			}
		}
	}

}

// Listens on the write channel and executes them on the
// endpoint.
func (s *session) handleInput() error {
	for rpc := range s.writeChan {
		if err := s.write(rpc); err != nil {
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

	var resp []byte
	if bytes.Contains(s.capabilities, []byte(netconf_1_1_capability)) {
		s.delimiter = []byte(delimiter_1_1)
		s.version = Netconf_Version_1_1
		resp = []byte(capabilities_1_1)
		if bytes.Contains(s.capabilities, []byte(netconf_1_0_capability)) {
			resp = []byte(capabilities_1_0_1_1)
			s.version = Netconf_Version_1_0_1_1
		}
	} else {
		s.delimiter = []byte(delimiter_1_0)
		resp = []byte(capabilities_1_0)
		s.version = Netconf_Version_1_0
	}
	_, err = s.writer.Write(resp)
	if err != nil {
		return err
	}
	return nil
}

// Writes the given rpc to the endpoint
func (s *session) write(b []byte) error {
	_, err := s.writer.Write(s.wrap(b))
	if err != nil {
		return err
	}
	return nil
}

// removes the suffixes and responses from the various delimiters
func (s *session) sanitize(b []byte) []byte {
	switch s.version {
	case Netconf_Version_1_0_1_1:
		fallthrough
	case Netconf_Version_1_1:
		// TODO Add code to remove suffix
	}
	return bytes.TrimSuffix(b, s.delimiter)
}

// Wraps the given rpc with the appropriate delimiters
func (s *session) wrap(b []byte) []byte {
	switch s.version {
	case Netconf_Version_1_0_1_1:
		fallthrough
	case Netconf_Version_1_1:
		b = append([]byte(fmt.Sprintf(suffix1_1, len(b))), b...)
		b = append(b, s.delimiter...)
	case Netconf_Version_1_0:
		b = append(b, s.delimiter...)
	}
	return b
}

// Reads from the given reader till the delimiter is reached
// and returns the response
func (s *session) read(reader io.Reader, delimiter []byte) ([]byte, error) {
	var d []byte
	var err error
	d, s.leftovers, err = ReadTillDelimiter(reader, delimiter, s.leftovers)
	return d, err
}

// Extracts the message id from the message
func (s *session) msgId(msg []byte) (int, error) {
	match := re.FindStringSubmatch(string(msg))
	if len(match) > 1 {
		id, err := strconv.Atoi(match[1])
		if err != nil {
			return 0, err
		}
		return id, nil
	}
	return 0, errors.New("msg id not found")
}
